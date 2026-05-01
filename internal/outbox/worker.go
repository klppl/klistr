package outbox

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrCircuitOpen is returned by delivery functions when a relay circuit breaker
// is open. The worker reschedules the item without incrementing attempts or
// logging a warning — the circuit will recover on its own.
var ErrCircuitOpen = errors.New("circuit open")

// ErrPermanentFailure is returned by delivery functions when a destination
// returns a permanent rejection error (e.g. "kind X not allowed").
// The worker dead-letters the item immediately without retrying.
var ErrPermanentFailure = errors.New("permanent failure")

// DeliverFunc is the signature for protocol-specific delivery logic.
type DeliverFunc func(ctx context.Context, item Item) error

// WorkerPoolConfig holds tuning parameters for the worker pool.
type WorkerPoolConfig struct {
	APWorkers    int
	RelayWorkers int
	BskyWorkers  int
	// PollInterval is the fallback poll period used when no wake-up signal
	// arrives. With Notifier wiring (see EnqueueAdapter), workers wake
	// immediately on Enqueue, so this only matters for picking up items whose
	// next_retry_at falls in the future (delayed retries).
	PollInterval time.Duration
}

// Notifier is the wake-up surface implemented by WorkerPool. EnqueueAdapter
// calls Notify after a successful Enqueue so the matching worker wakes
// immediately instead of waiting for the fallback poll tick.
type Notifier interface {
	Notify(destType string)
}

// WorkerPool drains the outbox queue with per-dest-type worker goroutines.
type WorkerPool struct {
	queue      *Queue
	config     WorkerPoolConfig
	deliverers map[string]DeliverFunc
	mu         sync.RWMutex

	// notify holds a buffered (cap=1) wake-up channel per dest_type.
	// Pre-populated for "ap", "relay", "bsky" so Notify is safe before Start.
	notify map[string]chan struct{}
}

// NewWorkerPool creates a new worker pool.
func NewWorkerPool(queue *Queue, config WorkerPoolConfig) *WorkerPool {
	if config.PollInterval == 0 {
		config.PollInterval = 5 * time.Second
	}
	return &WorkerPool{
		queue:      queue,
		config:     config,
		deliverers: make(map[string]DeliverFunc),
		notify: map[string]chan struct{}{
			"ap":    make(chan struct{}, 1),
			"relay": make(chan struct{}, 1),
			"bsky":  make(chan struct{}, 1),
		},
	}
}

// RegisterDeliverer sets the delivery function for a dest_type.
func (wp *WorkerPool) RegisterDeliverer(destType string, fn DeliverFunc) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.deliverers[destType] = fn
}

// Notify wakes up any sleeping workers for destType so they re-poll the queue
// immediately. Safe to call from any goroutine; non-blocking.
func (wp *WorkerPool) Notify(destType string) {
	wp.mu.RLock()
	ch := wp.notify[destType]
	wp.mu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Start launches all worker goroutines. Non-blocking — returns immediately.
// Workers run until ctx is cancelled.
func (wp *WorkerPool) Start(ctx context.Context) {
	launch := func(destType string, count int) {
		for i := 0; i < count; i++ {
			go wp.runWorker(ctx, destType, i)
		}
	}

	launch("ap", wp.config.APWorkers)
	launch("relay", wp.config.RelayWorkers)
	launch("bsky", wp.config.BskyWorkers)
}

func (wp *WorkerPool) runWorker(ctx context.Context, destType string, workerID int) {
	slog.Debug("outbox worker started", "dest_type", destType, "worker", workerID)
	defer slog.Debug("outbox worker stopped", "dest_type", destType, "worker", workerID)

	wp.mu.RLock()
	notify := wp.notify[destType]
	wp.mu.RUnlock()

	ticker := time.NewTicker(wp.config.PollInterval)
	defer ticker.Stop()

	for {
		// Drain all available work without sleeping. Claim is now cheap when
		// the queue is empty (single non-tx SELECT) so this loop exits fast.
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			item, ok, err := wp.queue.Claim(destType)
			if err != nil {
				// SQLITE_BUSY is expected contention with multiple workers — not actionable.
				slog.Debug("outbox claim contention", "dest_type", destType, "error", err)
				break
			}
			if !ok {
				break
			}

			wp.deliver(ctx, item)
		}

		// Sleep until notified or the fallback ticker fires.
		select {
		case <-ctx.Done():
			return
		case <-notify:
		case <-ticker.C:
		}
	}
}

func (wp *WorkerPool) deliver(ctx context.Context, item Item) {
	wp.mu.RLock()
	fn, ok := wp.deliverers[item.DestType]
	wp.mu.RUnlock()

	if !ok {
		slog.Warn("outbox: no deliverer for dest_type", "dest_type", item.DestType, "id", item.ID)
		wp.queue.Fail(item.ID, "no deliverer registered for "+item.DestType)
		return
	}

	err := fn(ctx, item)
	if err != nil {
		if errors.Is(err, ErrCircuitOpen) {
			// Circuit is open — reschedule silently without burning an attempt.
			// The circuit breaker will recover on its own (5 min cooldown).
			wp.queue.Reschedule(item.ID, 5*time.Minute)
			return
		}
		if errors.Is(err, ErrPermanentFailure) {
			slog.Warn("outbox permanent failure (dead-lettering)",
				"dest_type", item.DestType,
				"dest_url", item.DestURL,
				"id", item.ID,
				"error", err,
			)
			if killErr := wp.queue.Kill(item.ID, err.Error()); killErr != nil {
				slog.Warn("outbox kill update error", "id", item.ID, "error", killErr)
			}
			return
		}
		slog.Warn("outbox delivery failed",
			"dest_type", item.DestType,
			"dest_url", item.DestURL,
			"id", item.ID,
			"attempt", item.Attempts+1,
			"error", err,
		)
		if failErr := wp.queue.Fail(item.ID, err.Error()); failErr != nil {
			slog.Warn("outbox fail update error", "id", item.ID, "error", failErr)
		}
		return
	}

	if err := wp.queue.Complete(item.ID); err != nil {
		slog.Warn("outbox complete update error", "id", item.ID, "error", err)
	}

	slog.Debug("outbox delivery succeeded",
		"dest_type", item.DestType,
		"dest_url", item.DestURL,
		"id", item.ID,
	)
}
