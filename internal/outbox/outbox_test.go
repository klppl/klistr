package outbox

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// testQueue creates an in-memory SQLite queue for testing.
// MaxOpenConns(1) is required because ":memory:" creates a separate DB per
// connection; limiting to one connection ensures all goroutines share the
// same in-memory database (same behavior as the production SQLite config).
func testQueue(t *testing.T) *Queue {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	q := NewQueue(db, "sqlite")
	if err := q.Migrate(); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestEnqueueAndClaim(t *testing.T) {
	q := testQueue(t)

	id, err := q.Enqueue(Item{
		DestType:      "ap",
		DestURL:       "https://mastodon.social/inbox",
		Payload:       `{"type":"Create"}`,
		Priority:      1,
		SourceEventID: "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected non-zero ID")
	}

	item, ok, err := q.Claim("ap")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected to claim an item")
	}
	if item.ID != id {
		t.Errorf("claimed ID = %d, want %d", item.ID, id)
	}
	if item.DestURL != "https://mastodon.social/inbox" {
		t.Errorf("dest_url = %q, want mastodon inbox", item.DestURL)
	}
	if item.Payload != `{"type":"Create"}` {
		t.Errorf("payload mismatch")
	}
	if item.Status != StatusClaimed {
		t.Errorf("status = %q, want %q", item.Status, StatusClaimed)
	}
}

func TestEnqueueDedupsBySourceEvent(t *testing.T) {
	q := testQueue(t)

	// First enqueue inserts.
	id1, err := q.Enqueue(Item{
		DestType:      "bsky",
		DestURL:       "https://bsky.social",
		Payload:       `{"first":true}`,
		SourceEventID: "evt-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id1 == 0 {
		t.Fatal("expected first enqueue to return a row id")
	}

	// Second enqueue with same (dest_type, dest_url, source_event_id) is a no-op.
	id2, err := q.Enqueue(Item{
		DestType:      "bsky",
		DestURL:       "https://bsky.social",
		Payload:       `{"duplicate":true}`,
		SourceEventID: "evt-1",
	})
	if err != nil {
		t.Fatalf("dedup enqueue should not error: %v", err)
	}
	if id2 != 0 {
		t.Errorf("dedup enqueue should return id=0, got %d", id2)
	}

	// Different dest_url with same source event: should insert (fan-out).
	id3, err := q.Enqueue(Item{
		DestType:      "relay",
		DestURL:       "wss://relay-a",
		Payload:       `{}`,
		SourceEventID: "evt-2",
	})
	if err != nil || id3 == 0 {
		t.Fatalf("fan-out target A should insert: id=%d err=%v", id3, err)
	}
	id4, err := q.Enqueue(Item{
		DestType:      "relay",
		DestURL:       "wss://relay-b",
		Payload:       `{}`,
		SourceEventID: "evt-2",
	})
	if err != nil || id4 == 0 {
		t.Fatalf("fan-out target B should insert: id=%d err=%v", id4, err)
	}

	// Empty SourceEventID bypasses dedup — two rows allowed.
	id5, err := q.Enqueue(Item{DestType: "ap", DestURL: "https://x", Payload: "{}"})
	if err != nil || id5 == 0 {
		t.Fatalf("empty-source enqueue 1 failed: id=%d err=%v", id5, err)
	}
	id6, err := q.Enqueue(Item{DestType: "ap", DestURL: "https://x", Payload: "{}"})
	if err != nil || id6 == 0 {
		t.Fatalf("empty-source enqueue 2 should also succeed (no dedup): id=%d err=%v", id6, err)
	}

	// Only the first (evt-1) plus evt-2 fan-out plus the two empty-source rows
	// should be present — 5 total, none lost or duplicated.
	stats, err := q.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if got := stats.Pending; got != 5 {
		t.Errorf("pending = %d, want 5 (evt-1 once + evt-2 fanned to two relays + 2 empty-source)", got)
	}
}

func TestClaimRespectsDestType(t *testing.T) {
	q := testQueue(t)

	q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com/inbox", Payload: "{}"})
	q.Enqueue(Item{DestType: "relay", DestURL: "wss://relay.damus.io", Payload: "{}"})

	item, ok, _ := q.Claim("relay")
	if !ok {
		t.Fatal("expected to claim relay item")
	}
	if item.DestType != "relay" {
		t.Errorf("dest_type = %q, want relay", item.DestType)
	}
}

func TestClaimRespectsPriority(t *testing.T) {
	q := testQueue(t)

	q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}", Priority: 2})
	q.Enqueue(Item{DestType: "ap", DestURL: "https://b.com", Payload: "{}", Priority: 0})

	item, ok, _ := q.Claim("ap")
	if !ok {
		t.Fatal("expected to claim")
	}
	if item.Priority != 0 {
		t.Errorf("claimed priority = %d, want 0 (real-time)", item.Priority)
	}
}

func TestClaimRespectsNextRetryAt(t *testing.T) {
	q := testQueue(t)

	q.Enqueue(Item{
		DestType:    "ap",
		DestURL:     "https://a.com",
		Payload:     "{}",
		NextRetryAt: time.Now().Add(1 * time.Hour),
	})

	_, ok, _ := q.Claim("ap")
	if ok {
		t.Fatal("should not claim item with future next_retry_at")
	}
}

func TestComplete(t *testing.T) {
	q := testQueue(t)

	id, _ := q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}"})
	q.Claim("ap")
	err := q.Complete(id)
	if err != nil {
		t.Fatal(err)
	}

	_, ok, _ := q.Claim("ap")
	if ok {
		t.Fatal("completed item should not be claimable")
	}
}

func TestFailAndRetry(t *testing.T) {
	q := testQueue(t)

	id, _ := q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}"})
	q.Claim("ap")
	err := q.Fail(id, "connection refused")
	if err != nil {
		t.Fatal(err)
	}

	item, err := q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != StatusPending {
		t.Errorf("status = %q, want pending", item.Status)
	}
	if item.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", item.Attempts)
	}
	if item.LastError != "connection refused" {
		t.Errorf("last_error = %q, want 'connection refused'", item.LastError)
	}
}

func TestFailExhaustsToDeadLetter(t *testing.T) {
	q := testQueue(t)

	id, _ := q.Enqueue(Item{
		DestType:    "ap",
		DestURL:     "https://a.com",
		Payload:     "{}",
		MaxAttempts: 2,
	})

	q.Claim("ap")
	q.Fail(id, "err1")
	q.forceRetryNow(id)
	q.Claim("ap")
	q.Fail(id, "err2")

	item, _ := q.Get(id)
	if item.Status != StatusDead {
		t.Errorf("status = %q, want dead after %d attempts", item.Status, item.Attempts)
	}
}

func TestStats(t *testing.T) {
	q := testQueue(t)

	id, _ := q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}", Priority: 0})
	q.Enqueue(Item{DestType: "ap", DestURL: "https://b.com", Payload: "{}"})
	q.Enqueue(Item{DestType: "relay", DestURL: "wss://r.com", Payload: "{}"})
	q.Enqueue(Item{DestType: "ap", DestURL: "https://c.com", Payload: "{}"})
	q.Claim("ap")
	q.Complete(id)

	stats, err := q.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 3 {
		t.Errorf("pending = %d, want 3", stats.Pending)
	}
	if stats.Done != 1 {
		t.Errorf("done = %d, want 1", stats.Done)
	}
}

func TestCleanup(t *testing.T) {
	q := testQueue(t)

	id, _ := q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}"})
	q.Claim("ap")
	q.Complete(id)

	removed, err := q.Cleanup(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
}

func TestRetryDead(t *testing.T) {
	q := testQueue(t)

	id, _ := q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}", MaxAttempts: 1})
	q.Claim("ap")
	q.Fail(id, "fatal")

	item, _ := q.Get(id)
	if item.Status != StatusDead {
		t.Fatalf("expected dead, got %q", item.Status)
	}

	err := q.RetryDead(id)
	if err != nil {
		t.Fatal(err)
	}

	item, _ = q.Get(id)
	if item.Status != StatusPending {
		t.Errorf("after retry: status = %q, want pending", item.Status)
	}
	if item.Attempts != 0 {
		t.Errorf("after retry: attempts = %d, want 0", item.Attempts)
	}
}

func TestKill(t *testing.T) {
	q := testQueue(t)

	id, _ := q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}", MaxAttempts: 5})
	err := q.Kill(id, "permanent rejection")
	if err != nil {
		t.Fatal(err)
	}

	item, _ := q.Get(id)
	if item.Status != StatusDead {
		t.Errorf("status = %q, want dead", item.Status)
	}
	if item.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", item.Attempts)
	}
	if item.LastError != "permanent rejection" {
		t.Errorf("last_error = %q, want 'permanent rejection'", item.LastError)
	}
}
