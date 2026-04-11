package outbox

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestFullOutboxFlow(t *testing.T) {
	q := testQueue(t)

	var apCount, relayCount atomic.Int32

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wp := NewWorkerPool(q, WorkerPoolConfig{
		APWorkers:    2,
		RelayWorkers: 1,
		BskyWorkers:  0,
		PollInterval: 50 * time.Millisecond,
	})

	wp.RegisterDeliverer("ap", func(ctx context.Context, item Item) error {
		apCount.Add(1)
		return nil
	})
	wp.RegisterDeliverer("relay", func(ctx context.Context, item Item) error {
		relayCount.Add(1)
		return nil
	})

	wp.Start(ctx)

	for i := 0; i < 5; i++ {
		q.Enqueue(Item{
			DestType: "ap",
			DestURL:  fmt.Sprintf("https://server%d.com/inbox", i),
			Payload:  `{"type":"Create"}`,
			Priority: PriorityNormal,
		})
	}
	for i := 0; i < 3; i++ {
		q.Enqueue(Item{
			DestType: "relay",
			DestURL:  fmt.Sprintf("wss://relay%d.com", i),
			Payload:  `{"id":"abc","kind":1}`,
			Priority: PriorityNormal,
		})
	}

	deadline := time.After(4 * time.Second)
	for apCount.Load() < 5 || relayCount.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("timed out: ap=%d relay=%d", apCount.Load(), relayCount.Load())
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Wait for Complete() calls to finish.
	time.Sleep(100 * time.Millisecond)
	stats, err := q.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 8 {
		t.Errorf("done = %d, want 8", stats.Done)
	}
	if stats.Pending != 0 {
		t.Errorf("pending = %d, want 0", stats.Pending)
	}
}

func TestDeadLetterFlow(t *testing.T) {
	q := testQueue(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wp := NewWorkerPool(q, WorkerPoolConfig{
		APWorkers:    1,
		PollInterval: 50 * time.Millisecond,
	})
	wp.RegisterDeliverer("ap", func(ctx context.Context, item Item) error {
		return fmt.Errorf("permanent failure")
	})
	wp.Start(ctx)

	q.Enqueue(Item{
		DestType:    "ap",
		DestURL:     "https://down.com/inbox",
		Payload:     `{}`,
		MaxAttempts: 1,
	})

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for dead letter")
		default:
		}
		stats, _ := q.Stats()
		if stats.Dead >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	dead, err := q.DeadLetters()
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dead))
	}
	if dead[0].LastError != "permanent failure" {
		t.Errorf("last_error = %q, want 'permanent failure'", dead[0].LastError)
	}
}
