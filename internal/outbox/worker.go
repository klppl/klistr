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

// DeliverFunc is the signature for protocol-specific delivery logic.
type DeliverFunc func(ctx context.Context, item Item) error

// WorkerPoolConfig holds tuning parameters for the worker pool.
type WorkerPoolConfig struct {
	APWorkers    int
	RelayWorkers int
	BskyWorkers  int
	PollInterval time.Duration
}

// WorkerPool drains the outbox queue with per-dest-type worker goroutines.
type WorkerPool struct {
	queue      *Queue
	config     WorkerPoolConfig
	deliverers map[string]DeliverFunc
	mu         sync.RWMutex
}

// NewWorkerPool creates a new worker pool.
func NewWorkerPool(queue *Queue, config WorkerPoolConfig) *WorkerPool {
	if config.PollInterval == 0 {
		config.PollInterval = 100 * time.Millisecond
	}
	return &WorkerPool{
		queue:      queue,
		config:     config,
		deliverers: make(map[string]DeliverFunc),
	}
}

// RegisterDeliverer sets the delivery function for a dest_type.
func (wp *WorkerPool) RegisterDeliverer(destType string, fn DeliverFunc) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.deliverers[destType] = fn
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
			wp.sleep(ctx)
			continue
		}
		if !ok {
			wp.sleep(ctx)
			continue
		}

		wp.deliver(ctx, item)
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

func (wp *WorkerPool) sleep(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(wp.config.PollInterval):
	}
}
