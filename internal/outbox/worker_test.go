package outbox

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerPoolDrainsItems(t *testing.T) {
	q := testQueue(t)

	var delivered atomic.Int32
	deliverFn := func(ctx context.Context, item Item) error {
		delivered.Add(1)
		return nil
	}

	q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}"})
	q.Enqueue(Item{DestType: "ap", DestURL: "https://b.com", Payload: "{}"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	wp := NewWorkerPool(q, WorkerPoolConfig{
		APWorkers:    1,
		RelayWorkers: 0,
		BskyWorkers:  0,
		PollInterval: 50 * time.Millisecond,
	})
	wp.RegisterDeliverer("ap", deliverFn)
	wp.Start(ctx)

	deadline := time.After(2 * time.Second)
	for delivered.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out: delivered %d, want 2", delivered.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
}

func TestWorkerPoolRetriesOnFailure(t *testing.T) {
	q := testQueue(t)

	var attempts atomic.Int32
	deliverFn := func(ctx context.Context, item Item) error {
		n := attempts.Add(1)
		if n == 1 {
			return fmt.Errorf("temporary failure")
		}
		return nil
	}

	id, _ := q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}", MaxAttempts: 3})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wp := NewWorkerPool(q, WorkerPoolConfig{
		APWorkers:    1,
		PollInterval: 50 * time.Millisecond,
	})
	wp.RegisterDeliverer("ap", deliverFn)
	wp.Start(ctx)

	// Wait for the first attempt to fail, then force retry time to past
	// (backoff would be 5s which is too long for tests).
	for i := 0; i < 200; i++ {
		time.Sleep(10 * time.Millisecond)
		if attempts.Load() >= 1 {
			q.forceRetryNow(id)
			break
		}
	}

	deadline := time.After(3 * time.Second)
	for attempts.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out: attempts = %d, want >= 2", attempts.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
}

// TestWorkerPoolStopsDrainingOnCircuitOpen reproduces the CPU-melt bug: when a
// destination's circuit is open, the relay deliverer returns ErrCircuitOpen for
// every item. The old worker kept claiming the next item in the same drain pass,
// spinning through the entire backlog (thousands of claim+reschedule cycles)
// every wake-up and pinning the CPU. The fix breaks the drain loop on the first
// circuit-open result so a backlog bound for a down destination costs ~one claim
// per poll tick instead of one per item.
func TestWorkerPoolStopsDrainingOnCircuitOpen(t *testing.T) {
	q := testQueue(t)

	var delivered atomic.Int32
	deliverFn := func(ctx context.Context, item Item) error {
		delivered.Add(1)
		return ErrCircuitOpen
	}

	// A backlog of items all bound for the (down) destination, all due now.
	const backlog = 50
	for i := 0; i < backlog; i++ {
		q.Enqueue(Item{DestType: "relay", DestURL: "wss://down.example", Payload: "{}"})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Large poll interval: only the initial drain pass runs within the test
	// window, so the count reflects a single wake-up.
	wp := NewWorkerPool(q, WorkerPoolConfig{
		RelayWorkers: 1,
		PollInterval: 10 * time.Second,
	})
	wp.RegisterDeliverer("relay", deliverFn)
	wp.Start(ctx)

	// Give the worker time to do its initial drain pass and go back to sleep.
	time.Sleep(300 * time.Millisecond)
	cancel()

	// With the fix the worker handles exactly one circuit-open item then breaks
	// to sleep. Without it, all 50 would be processed in the single pass. Allow a
	// small margin for scheduling, but it must be far below the backlog size.
	if n := delivered.Load(); n > 5 {
		t.Fatalf("worker churned on circuit-open backlog: %d deliveries in one pass (want <= 5 of %d)", n, backlog)
	}

	// The circuit-open item must have been rescheduled, not dead-lettered (it is
	// not yet stale), so the backlog is preserved for when the circuit recovers.
	stats, err := q.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Dead != 0 {
		t.Fatalf("fresh circuit-open item was dead-lettered: %d dead", stats.Dead)
	}
}

// TestWorkerPoolDeadLettersStaleCircuitOpenItem verifies the backlog cap: an
// item that has been undeliverable (circuit open) for longer than
// maxCircuitOpenAge is dead-lettered instead of rescheduled forever, so a
// destination that stays down can't grow an unbounded backlog.
func TestWorkerPoolDeadLettersStaleCircuitOpenItem(t *testing.T) {
	q := testQueue(t)

	deliverFn := func(ctx context.Context, item Item) error {
		return ErrCircuitOpen
	}

	// Enqueue an item created well beyond the max age.
	q.Enqueue(Item{
		DestType:  "relay",
		DestURL:   "wss://long-dead.example",
		Payload:   "{}",
		CreatedAt: time.Now().Add(-2 * maxCircuitOpenAge),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wp := NewWorkerPool(q, WorkerPoolConfig{
		RelayWorkers: 1,
		PollInterval: 50 * time.Millisecond,
	})
	wp.RegisterDeliverer("relay", deliverFn)
	wp.Start(ctx)

	deadline := time.After(3 * time.Second)
	for {
		stats, err := q.Stats()
		if err != nil {
			t.Fatal(err)
		}
		if stats.Dead == 1 {
			break // success: stale item was dead-lettered
		}
		select {
		case <-deadline:
			t.Fatalf("stale circuit-open item was not dead-lettered: %+v", stats)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
}

func TestWorkerPoolRespectsPriority(t *testing.T) {
	q := testQueue(t)

	// Use a channel to avoid data race on slice append from worker goroutine.
	orderCh := make(chan int, 2)
	deliverFn := func(ctx context.Context, item Item) error {
		orderCh <- item.Priority
		return nil
	}

	q.Enqueue(Item{DestType: "ap", DestURL: "https://bg.com", Payload: "{}", Priority: PriorityBackground})
	q.Enqueue(Item{DestType: "ap", DestURL: "https://rt.com", Payload: "{}", Priority: PriorityRealTime})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	wp := NewWorkerPool(q, WorkerPoolConfig{
		APWorkers:    1, // single worker to force serial processing
		PollInterval: 50 * time.Millisecond,
	})
	wp.RegisterDeliverer("ap", deliverFn)
	wp.Start(ctx)

	var order []int
	deadline := time.After(2 * time.Second)
	for len(order) < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out: delivered %d, want 2", len(order))
		case p := <-orderCh:
			order = append(order, p)
		}
	}

	if order[0] != PriorityRealTime {
		t.Errorf("first delivery priority = %d, want %d (real-time)", order[0], PriorityRealTime)
	}
	cancel()
}
