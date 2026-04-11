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
