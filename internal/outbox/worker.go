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

// ErrDropSilently is returned by delivery functions when an item can never be
// delivered for a reason that is already known and non-actionable — e.g. the
// relay is known to restrict this event kind. The worker marks the item done
// (removing it from the queue) without dead-lettering or logging a warning,
// avoiding log floods and dead-letter-table pollution when a large backlog of
// such items drains at once.
var ErrDropSilently = errors.New("drop silently")

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

// reapStaleAge is the threshold after which a status='claimed' row is assumed
// abandoned by a crashed worker and flipped back to pending. Set well above
// any sane delivery time (AP HTTP delivery has its own short context) but low
// enough that crash recovery doesn't have to wait long.
const reapStaleAge = 5 * time.Minute

// reapStaleInterval is how often the reaper goroutine checks for abandoned rows.
const reapStaleInterval = 60 * time.Second

// circuitOpenBackoff is how far out a circuit-open item is rescheduled. It must
// be comfortably larger than the worker's poll interval so a backlog of items
// bound for a down destination leaves the "due now" set quickly instead of
// being re-claimed on the next tick.
const circuitOpenBackoff = 5 * time.Minute

// maxCircuitOpenAge bounds how long an item may stay undeliverable purely
// because its destination's circuit is open. Past this age the item is dead-
// lettered instead of rescheduled. Without this cap a destination that stays
// down (e.g. a dead relay) grows an unbounded backlog — circuit-open
// reschedules never increment attempts, so such items would otherwise never
// age out, and the ever-growing due set pins the CPU.
const maxCircuitOpenAge = 24 * time.Hour

// Start launches all worker goroutines. Non-blocking — returns immediately.
// Workers run until ctx is cancelled. Also starts a single reaper goroutine
// that periodically calls Queue.ReapStale to recover rows abandoned by crashed
// workers (rows stuck in status='claimed').
func (wp *WorkerPool) Start(ctx context.Context) {
	launch := func(destType string, count int) {
		for i := 0; i < count; i++ {
			go wp.runWorker(ctx, destType, i)
		}
	}

	launch("ap", wp.config.APWorkers)
	launch("relay", wp.config.RelayWorkers)
	launch("bsky", wp.config.BskyWorkers)

	go wp.runReaper(ctx)
}

// runReaper periodically reclaims rows stuck in status='claimed' beyond
// reapStaleAge so a worker crash mid-delivery doesn't strand items forever.
func (wp *WorkerPool) runReaper(ctx context.Context) {
	ticker := time.NewTicker(reapStaleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := wp.queue.ReapStale(reapStaleAge)
			if err != nil {
				slog.Warn("outbox reaper failed", "error", err)
				continue
			}
			if n > 0 {
				slog.Info("outbox reaper recovered abandoned rows", "count", n)
				// Wake every worker pool so the recovered rows are picked up
				// without waiting for the fallback poll tick.
				wp.Notify("ap")
				wp.Notify("relay")
				wp.Notify("bsky")
			}
		}
	}
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

			if wp.deliver(ctx, item) {
				// A destination on this lane has its circuit open. Stop draining
				// and sleep: continuing would spin through the entire backlog of
				// items bound for the same down destination, pinning the CPU.
				// The item we just hit was rescheduled out of the due set, and a
				// notify (on the next enqueue) or the fallback tick re-checks the
				// lane shortly — so healthy destinations resume promptly.
				break
			}
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

// deliver runs the registered deliverer for an item and records the outcome.
// It returns true when the item's destination has its circuit open, signalling
// the worker to stop draining this lane (see runWorker) so a backlog bound for
// a down destination can't spin the CPU.
func (wp *WorkerPool) deliver(ctx context.Context, item Item) (circuitOpen bool) {
	wp.mu.RLock()
	fn, ok := wp.deliverers[item.DestType]
	wp.mu.RUnlock()

	if !ok {
		slog.Warn("outbox: no deliverer for dest_type", "dest_type", item.DestType, "id", item.ID)
		wp.queue.Fail(item.ID, "no deliverer registered for "+item.DestType)
		return false
	}

	err := fn(ctx, item)
	if err != nil {
		if errors.Is(err, ErrCircuitOpen) {
			// Circuit is open. Don't burn an attempt — but give up entirely on
			// items that have been undeliverable for too long, so a destination
			// that stays down can't grow an unbounded backlog.
			if !item.CreatedAt.IsZero() && time.Since(item.CreatedAt) > maxCircuitOpenAge {
				slog.Warn("outbox: dropping stale item, destination unreachable too long",
					"dest_type", item.DestType,
					"dest_url", item.DestURL,
					"id", item.ID,
					"age", time.Since(item.CreatedAt).Round(time.Minute),
				)
				if killErr := wp.queue.Kill(item.ID, "circuit open beyond max age: "+item.DestURL); killErr != nil {
					slog.Warn("outbox kill update error", "id", item.ID, "error", killErr)
				}
			} else {
				wp.queue.Reschedule(item.ID, circuitOpenBackoff)
			}
			return true
		}
		if errors.Is(err, ErrDropSilently) {
			// Known, non-actionable: drop without warning or dead-lettering.
			slog.Debug("outbox dropping undeliverable item",
				"dest_type", item.DestType,
				"dest_url", item.DestURL,
				"id", item.ID,
				"reason", err,
			)
			if doneErr := wp.queue.Complete(item.ID); doneErr != nil {
				slog.Warn("outbox drop update error", "id", item.ID, "error", doneErr)
			}
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
	return false
}
