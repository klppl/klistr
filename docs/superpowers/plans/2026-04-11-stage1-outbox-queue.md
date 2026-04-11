# Stage 1: Unified Outbox Queue — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace all three fire-and-forget delivery paths (AP federation, Nostr relay publish, Bluesky XRPC) with a crash-safe, DB-backed outbox queue that retries failed deliveries with exponential backoff and priority lanes.

**Architecture:** A new `internal/outbox/` package owns the queue table and worker pool. The existing `Federator.Federate()`, `Publisher.Publish()`, and `Poster.Handle()` are refactored to enqueue work instead of delivering directly. Per-dest-type worker goroutines drain the queue, calling the existing delivery functions. The relay circuit breaker becomes a fast-fail gate within the relay worker.

**Tech Stack:** Go 1.24, `database/sql` (SQLite via modernc.org/sqlite, PostgreSQL via lib/pq), existing `go-nostr` and `go-fed/httpsig` libraries.

---

## File Structure

### New Files
| File | Responsibility |
|------|---------------|
| `internal/outbox/outbox.go` | `Queue` struct, DB operations: `Enqueue`, `Claim`, `Complete`, `Fail`, `DeadLetter`, `Stats`, `RetryDead`, `Cleanup` |
| `internal/outbox/outbox_test.go` | Table-driven tests for all Queue operations |
| `internal/outbox/worker.go` | `WorkerPool` struct, per-dest-type drain goroutines, delivery dispatch |
| `internal/outbox/worker_test.go` | Worker pool tests with mock deliverers |
| `internal/outbox/backoff.go` | Backoff calculation with two profiles (default, thread-resolution) |
| `internal/outbox/backoff_test.go` | Table-driven backoff tests |

### Modified Files
| File | Changes |
|------|---------|
| `internal/db/db.go` | Append outbox table + index to `commonMigrations`, expose `DB()` accessor for outbox package |
| `internal/ap/federation.go` | `Federate()` gains an `Enqueuer` field; when set, enqueues per-inbox instead of direct HTTP delivery. Inbox resolution stays in Federator. |
| `internal/nostr/relay.go` | `Publisher` gains an `Enqueuer` field; when set, `Publish()` enqueues per-relay instead of calling `PublishMany`. Circuit breaker stays for worker use. |
| `internal/bsky/poster.go` | `Poster` gains an `Enqueuer` field; when set, `Handle()` serializes the event+metadata and enqueues instead of direct XRPC calls. |
| `cmd/klistr/main.go` | Create `outbox.Queue`, create `WorkerPool` with delivery functions, wire `Enqueuer` into Federator/Publisher/Poster, start workers and cleanup goroutine. |
| `internal/server/admin.go` | Add outbox stats to the dashboard API response. Add dead-letter list endpoint and manual retry endpoint. |

---

## Task 1: Outbox Table Migration

**Files:**
- Modify: `internal/db/db.go:93-126` (append to `commonMigrations`)
- Modify: `internal/db/db.go` (add `DB()` accessor)

- [ ] **Step 1: Add outbox table to commonMigrations**

Open `internal/db/db.go` and append the following entries to the `commonMigrations` slice (after the `audit_log_ts` index on line 125):

```go
	// Outbox queue for at-least-once delivery across all three protocols.
	`CREATE TABLE IF NOT EXISTS outbox (
		id              INTEGER PRIMARY KEY,
		dest_type       TEXT NOT NULL,
		dest_url        TEXT NOT NULL,
		payload         TEXT NOT NULL,
		priority        INTEGER NOT NULL DEFAULT 1,
		status          TEXT NOT NULL DEFAULT 'pending',
		attempts        INTEGER NOT NULL DEFAULT 0,
		max_attempts    INTEGER NOT NULL DEFAULT 6,
		next_retry_at   TEXT NOT NULL,
		last_error      TEXT,
		created_at      TEXT NOT NULL,
		completed_at    TEXT,
		source_event_id TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS outbox_drain ON outbox(status, next_retry_at, priority)`,
```

Note: We use `INTEGER PRIMARY KEY` (not AUTOINCREMENT) because SQLite treats this as an alias for rowid with auto-increment behavior, and it's compatible with PostgreSQL's `SERIAL` via the existing migration approach (both drivers run the same DDL and PostgreSQL treats `INTEGER PRIMARY KEY` with a default sequence).

- [ ] **Step 2: Add DB() accessor to Store**

Add this method to `internal/db/db.go` near the bottom (before `ph()`):

```go
// DB returns the underlying *sql.DB for use by packages that need direct
// access (e.g. outbox queue). The caller must respect the Store's driver
// and connection constraints.
func (s *Store) DB() *sql.DB { return s.db }

// Driver returns "sqlite" or "postgres".
func (s *Store) Driver() string { return s.driver }
```

- [ ] **Step 3: Run migrations to verify**

Run: `cd /home/alex/GitHub/klistr && go build ./cmd/klistr`
Expected: Compiles successfully.

- [ ] **Step 4: Commit**

```bash
git add internal/db/db.go
git commit -m "db: add outbox table migration and DB/Driver accessors"
```

---

## Task 2: Backoff Calculation

**Files:**
- Create: `internal/outbox/backoff.go`
- Create: `internal/outbox/backoff_test.go`

- [ ] **Step 1: Write the failing backoff tests**

Create `internal/outbox/backoff_test.go`:

```go
package outbox

import (
	"testing"
	"time"
)

func TestDefaultBackoff(t *testing.T) {
	tests := []struct {
		name     string
		attempt  int
		expected time.Duration
	}{
		{"attempt 1 is immediate", 1, 0},
		{"attempt 2 is 5s", 2, 5 * time.Second},
		{"attempt 3 is 30s", 3, 30 * time.Second},
		{"attempt 4 is 2m", 4, 2 * time.Minute},
		{"attempt 5 is 10m", 5, 10 * time.Minute},
		{"attempt 6 is 1h", 6, 1 * time.Hour},
		{"attempt 0 clamps to immediate", 0, 0},
		{"attempt 99 clamps to 1h", 99, 1 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DefaultBackoff(tc.attempt)
			if got != tc.expected {
				t.Errorf("DefaultBackoff(%d) = %v, want %v", tc.attempt, got, tc.expected)
			}
		})
	}
}

func TestThreadBackoff(t *testing.T) {
	tests := []struct {
		name     string
		attempt  int
		expected time.Duration
	}{
		{"attempt 1 is immediate", 1, 0},
		{"attempt 2 is 5s", 2, 5 * time.Second},
		{"attempt 3 is 30s", 3, 30 * time.Second},
		{"attempt 0 clamps to immediate", 0, 0},
		{"attempt 99 clamps to 30s", 99, 30 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ThreadBackoff(tc.attempt)
			if got != tc.expected {
				t.Errorf("ThreadBackoff(%d) = %v, want %v", tc.attempt, got, tc.expected)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/alex/GitHub/klistr && go test ./internal/outbox/...`
Expected: FAIL — package doesn't exist yet.

- [ ] **Step 3: Implement backoff functions**

Create `internal/outbox/backoff.go`:

```go
package outbox

import "time"

// defaultSchedule defines the delay before each retry attempt.
// Index 0 = attempt 1 (immediate), index 5 = attempt 6 (1 hour).
var defaultSchedule = []time.Duration{
	0,                  // attempt 1: immediate
	5 * time.Second,    // attempt 2
	30 * time.Second,   // attempt 3
	2 * time.Minute,    // attempt 4
	10 * time.Minute,   // attempt 5
	1 * time.Hour,      // attempt 6
}

// threadSchedule is a compressed backoff for thread-resolution retries
// where the parent post typically arrives within seconds.
var threadSchedule = []time.Duration{
	0,               // attempt 1: immediate
	5 * time.Second, // attempt 2
	30 * time.Second, // attempt 3
}

// DefaultBackoff returns the delay before the given attempt number (1-based).
// Attempts beyond the schedule length return the last entry.
func DefaultBackoff(attempt int) time.Duration {
	return backoffFor(defaultSchedule, attempt)
}

// ThreadBackoff returns the delay for thread-resolution retries.
// Shorter schedule since parent propagation is typically fast.
func ThreadBackoff(attempt int) time.Duration {
	return backoffFor(threadSchedule, attempt)
}

func backoffFor(schedule []time.Duration, attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	idx := attempt - 1
	if idx >= len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[idx]
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /home/alex/GitHub/klistr && go test ./internal/outbox/... -v`
Expected: All 13 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/outbox/backoff.go internal/outbox/backoff_test.go
git commit -m "outbox: add backoff calculation with default and thread-resolution profiles"
```

---

## Task 3: Queue CRUD Operations

**Files:**
- Create: `internal/outbox/outbox.go`
- Create: `internal/outbox/outbox_test.go`

- [ ] **Step 1: Write failing Queue tests**

Create `internal/outbox/outbox_test.go`:

```go
package outbox

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// testQueue creates an in-memory SQLite queue for testing.
func testQueue(t *testing.T) *Queue {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
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

func TestClaimRespectsDestType(t *testing.T) {
	q := testQueue(t)

	q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com/inbox", Payload: "{}"})
	q.Enqueue(Item{DestType: "relay", DestURL: "wss://relay.damus.io", Payload: "{}"})

	// Claiming "relay" should only get the relay item.
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

	// Enqueue low priority first, then high priority.
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

	// Enqueue an item with next_retry_at in the future.
	q.Enqueue(Item{
		DestType:    "ap",
		DestURL:     "https://a.com",
		Payload:     "{}",
		NextRetryAt: time.Now().Add(1 * time.Hour),
	})

	// Should not be claimable yet.
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

	// Should not be claimable again.
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

	// Item should become pending again with attempts=1 and a future next_retry_at.
	// But since DefaultBackoff(2) = 5s, it's in the near future. Wait or check directly.
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

	// Attempt 1: claim and fail.
	q.Claim("ap")
	q.Fail(id, "err1")

	// Force next_retry_at to past so we can claim again.
	q.forceRetryNow(id)

	// Attempt 2: claim and fail — should become dead.
	q.Claim("ap")
	q.Fail(id, "err2")

	item, _ := q.Get(id)
	if item.Status != StatusDead {
		t.Errorf("status = %q, want dead after %d attempts", item.Status, item.Attempts)
	}
}

func TestStats(t *testing.T) {
	q := testQueue(t)

	q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}"})
	q.Enqueue(Item{DestType: "ap", DestURL: "https://b.com", Payload: "{}"})
	q.Enqueue(Item{DestType: "relay", DestURL: "wss://r.com", Payload: "{}"})

	id, _ := q.Enqueue(Item{DestType: "ap", DestURL: "https://c.com", Payload: "{}"})
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

	// Cleanup with 0 TTL should remove it immediately.
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/alex/GitHub/klistr && go test ./internal/outbox/... -run TestEnqueue`
Expected: FAIL — `Queue` type not defined.

- [ ] **Step 3: Implement the Queue**

Create `internal/outbox/outbox.go`:

```go
package outbox

import (
	"database/sql"
	"fmt"
	"time"
)

// Status constants for outbox items.
const (
	StatusPending = "pending"
	StatusClaimed = "claimed"
	StatusDone    = "done"
	StatusDead    = "dead"
)

// Priority constants.
const (
	PriorityRealTime   = 0
	PriorityNormal     = 1
	PriorityBackground = 2
)

// Item represents a single outbox delivery task.
type Item struct {
	ID            int64
	DestType      string // "ap", "relay", "bsky"
	DestURL       string
	Payload       string
	Priority      int
	Status        string
	Attempts      int
	MaxAttempts   int
	NextRetryAt   time.Time
	LastError     string
	CreatedAt     time.Time
	CompletedAt   *time.Time
	SourceEventID string
}

// QueueStats holds aggregate counts for the admin UI.
type QueueStats struct {
	Pending int `json:"pending"`
	Claimed int `json:"claimed"`
	Done    int `json:"done"`
	Dead    int `json:"dead"`
}

// Queue manages the outbox table for at-least-once delivery.
type Queue struct {
	db     *sql.DB
	driver string
}

// NewQueue creates a new Queue backed by the given database.
func NewQueue(db *sql.DB, driver string) *Queue {
	return &Queue{db: db, driver: driver}
}

// Migrate creates the outbox table if it doesn't exist.
// This is also called from db.Store.Migrate via commonMigrations,
// but having it here lets tests create the table independently.
func (q *Queue) Migrate() error {
	_, err := q.db.Exec(`CREATE TABLE IF NOT EXISTS outbox (
		id              INTEGER PRIMARY KEY,
		dest_type       TEXT NOT NULL,
		dest_url        TEXT NOT NULL,
		payload         TEXT NOT NULL,
		priority        INTEGER NOT NULL DEFAULT 1,
		status          TEXT NOT NULL DEFAULT 'pending',
		attempts        INTEGER NOT NULL DEFAULT 0,
		max_attempts    INTEGER NOT NULL DEFAULT 6,
		next_retry_at   TEXT NOT NULL,
		last_error      TEXT,
		created_at      TEXT NOT NULL,
		completed_at    TEXT,
		source_event_id TEXT
	)`)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(`CREATE INDEX IF NOT EXISTS outbox_drain ON outbox(status, next_retry_at, priority)`)
	return err
}

// Enqueue adds an item to the outbox. Returns the row ID.
func (q *Queue) Enqueue(item Item) (int64, error) {
	now := time.Now().UTC()
	if item.MaxAttempts == 0 {
		item.MaxAttempts = 6
	}
	if item.NextRetryAt.IsZero() {
		item.NextRetryAt = now
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}

	var query string
	if q.driver == "sqlite" {
		query = `INSERT INTO outbox (dest_type, dest_url, payload, priority, status, attempts, max_attempts, next_retry_at, created_at, source_event_id)
			VALUES (?, ?, ?, ?, 'pending', 0, ?, ?, ?, ?)`
	} else {
		query = `INSERT INTO outbox (dest_type, dest_url, payload, priority, status, attempts, max_attempts, next_retry_at, created_at, source_event_id)
			VALUES ($1, $2, $3, $4, 'pending', 0, $5, $6, $7, $8)`
	}

	result, err := q.db.Exec(query,
		item.DestType,
		item.DestURL,
		item.Payload,
		item.Priority,
		item.MaxAttempts,
		item.NextRetryAt.Format(time.RFC3339Nano),
		item.CreatedAt.Format(time.RFC3339Nano),
		item.SourceEventID,
	)
	if err != nil {
		return 0, fmt.Errorf("outbox enqueue: %w", err)
	}
	return result.LastInsertId()
}

// Claim atomically selects the highest-priority pending item for the given
// dest_type whose next_retry_at has passed, and marks it as claimed.
// Returns (item, true, nil) on success, (zero, false, nil) if nothing to claim.
func (q *Queue) Claim(destType string) (Item, bool, error) {
	tx, err := q.db.Begin()
	if err != nil {
		return Item{}, false, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Find the best candidate.
	var selectQ string
	if q.driver == "sqlite" {
		selectQ = `SELECT id FROM outbox
			WHERE status = 'pending' AND dest_type = ? AND next_retry_at <= ?
			ORDER BY priority, next_retry_at LIMIT 1`
	} else {
		selectQ = `SELECT id FROM outbox
			WHERE status = 'pending' AND dest_type = $1 AND next_retry_at <= $2
			ORDER BY priority, next_retry_at LIMIT 1
			FOR UPDATE SKIP LOCKED`
	}

	var id int64
	err = tx.QueryRow(selectQ, destType, now).Scan(&id)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("outbox claim select: %w", err)
	}

	// Mark as claimed.
	var updateQ string
	if q.driver == "sqlite" {
		updateQ = `UPDATE outbox SET status = 'claimed' WHERE id = ?`
	} else {
		updateQ = `UPDATE outbox SET status = 'claimed' WHERE id = $1`
	}
	if _, err := tx.Exec(updateQ, id); err != nil {
		return Item{}, false, fmt.Errorf("outbox claim update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Item{}, false, err
	}

	item, err := q.Get(id)
	return item, err == nil, err
}

// Complete marks an item as done.
func (q *Queue) Complete(id int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var query string
	if q.driver == "sqlite" {
		query = `UPDATE outbox SET status = 'done', completed_at = ? WHERE id = ?`
	} else {
		query = `UPDATE outbox SET status = 'done', completed_at = $1 WHERE id = $2`
	}
	_, err := q.db.Exec(query, now, id)
	return err
}

// Fail records a delivery failure. If attempts reach max_attempts, the item
// is dead-lettered. Otherwise it goes back to pending with the next backoff delay.
func (q *Queue) Fail(id int64, errMsg string) error {
	item, err := q.Get(id)
	if err != nil {
		return err
	}

	newAttempts := item.Attempts + 1
	if newAttempts >= item.MaxAttempts {
		// Dead letter.
		var query string
		if q.driver == "sqlite" {
			query = `UPDATE outbox SET status = 'dead', attempts = ?, last_error = ? WHERE id = ?`
		} else {
			query = `UPDATE outbox SET status = 'dead', attempts = $1, last_error = $2 WHERE id = $3`
		}
		_, err := q.db.Exec(query, newAttempts, errMsg, id)
		return err
	}

	// Schedule retry.
	delay := DefaultBackoff(newAttempts + 1) // +1 because attempt is 1-based for backoff
	nextRetry := time.Now().UTC().Add(delay).Format(time.RFC3339Nano)

	var query string
	if q.driver == "sqlite" {
		query = `UPDATE outbox SET status = 'pending', attempts = ?, last_error = ?, next_retry_at = ? WHERE id = ?`
	} else {
		query = `UPDATE outbox SET status = 'pending', attempts = $1, last_error = $2, next_retry_at = $3 WHERE id = $4`
	}
	_, err = q.db.Exec(query, newAttempts, errMsg, nextRetry, id)
	return err
}

// Get retrieves a single outbox item by ID.
func (q *Queue) Get(id int64) (Item, error) {
	var query string
	if q.driver == "sqlite" {
		query = `SELECT id, dest_type, dest_url, payload, priority, status, attempts, max_attempts,
			next_retry_at, COALESCE(last_error, ''), created_at, completed_at, COALESCE(source_event_id, '')
			FROM outbox WHERE id = ?`
	} else {
		query = `SELECT id, dest_type, dest_url, payload, priority, status, attempts, max_attempts,
			next_retry_at, COALESCE(last_error, ''), created_at, completed_at, COALESCE(source_event_id, '')
			FROM outbox WHERE id = $1`
	}

	var item Item
	var nextRetry, createdAt string
	var completedAt sql.NullString

	err := q.db.QueryRow(query, id).Scan(
		&item.ID, &item.DestType, &item.DestURL, &item.Payload,
		&item.Priority, &item.Status, &item.Attempts, &item.MaxAttempts,
		&nextRetry, &item.LastError, &createdAt, &completedAt, &item.SourceEventID,
	)
	if err != nil {
		return Item{}, fmt.Errorf("outbox get: %w", err)
	}

	item.NextRetryAt, _ = time.Parse(time.RFC3339Nano, nextRetry)
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	if completedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, completedAt.String)
		item.CompletedAt = &t
	}
	return item, nil
}

// Stats returns aggregate counts by status.
func (q *Queue) Stats() (QueueStats, error) {
	var stats QueueStats
	rows, err := q.db.Query(`SELECT status, COUNT(*) FROM outbox GROUP BY status`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return stats, err
		}
		switch status {
		case StatusPending:
			stats.Pending = count
		case StatusClaimed:
			stats.Claimed = count
		case StatusDone:
			stats.Done = count
		case StatusDead:
			stats.Dead = count
		}
	}
	return stats, rows.Err()
}

// RetryDead resets a dead-lettered item back to pending with zero attempts.
func (q *Queue) RetryDead(id int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var query string
	if q.driver == "sqlite" {
		query = `UPDATE outbox SET status = 'pending', attempts = 0, next_retry_at = ? WHERE id = ? AND status = 'dead'`
	} else {
		query = `UPDATE outbox SET status = 'pending', attempts = 0, next_retry_at = $1 WHERE id = $2 AND status = 'dead'`
	}
	result, err := q.db.Exec(query, now, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("item %d is not dead-lettered", id)
	}
	return nil
}

// Cleanup removes old completed and dead items. Returns total rows removed.
func (q *Queue) Cleanup(doneTTL, deadTTL time.Duration) (int64, error) {
	doneCutoff := time.Now().UTC().Add(-doneTTL).Format(time.RFC3339Nano)
	deadCutoff := time.Now().UTC().Add(-deadTTL).Format(time.RFC3339Nano)

	var total int64

	var q1 string
	if q.driver == "sqlite" {
		q1 = `DELETE FROM outbox WHERE status = 'done' AND completed_at < ?`
	} else {
		q1 = `DELETE FROM outbox WHERE status = 'done' AND completed_at < $1`
	}
	result, err := q.db.Exec(q1, doneCutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	total += n

	var q2 string
	if q.driver == "sqlite" {
		q2 = `DELETE FROM outbox WHERE status = 'dead' AND created_at < ?`
	} else {
		q2 = `DELETE FROM outbox WHERE status = 'dead' AND created_at < $1`
	}
	result, err = q.db.Exec(q2, deadCutoff)
	if err != nil {
		return total, err
	}
	n, _ = result.RowsAffected()
	total += n
	return total, nil
}

// DeadLetters returns all dead-lettered items, most recent first.
func (q *Queue) DeadLetters() ([]Item, error) {
	rows, err := q.db.Query(`SELECT id, dest_type, dest_url, payload, priority, status, attempts, max_attempts,
		next_retry_at, COALESCE(last_error, ''), created_at, completed_at, COALESCE(source_event_id, '')
		FROM outbox WHERE status = 'dead' ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var item Item
		var nextRetry, createdAt string
		var completedAt sql.NullString
		if err := rows.Scan(
			&item.ID, &item.DestType, &item.DestURL, &item.Payload,
			&item.Priority, &item.Status, &item.Attempts, &item.MaxAttempts,
			&nextRetry, &item.LastError, &createdAt, &completedAt, &item.SourceEventID,
		); err != nil {
			return nil, err
		}
		item.NextRetryAt, _ = time.Parse(time.RFC3339Nano, nextRetry)
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		if completedAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, completedAt.String)
			item.CompletedAt = &t
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// forceRetryNow is a test helper that sets next_retry_at to the past.
func (q *Queue) forceRetryNow(id int64) {
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	q.db.Exec(`UPDATE outbox SET next_retry_at = ? WHERE id = ?`, past, id)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /home/alex/GitHub/klistr && go test ./internal/outbox/... -v`
Expected: All tests PASS (backoff + queue tests).

- [ ] **Step 5: Commit**

```bash
git add internal/outbox/outbox.go internal/outbox/outbox_test.go
git commit -m "outbox: add Queue with Enqueue/Claim/Complete/Fail/Stats/Cleanup operations"
```

---

## Task 4: Worker Pool

**Files:**
- Create: `internal/outbox/worker.go`
- Create: `internal/outbox/worker_test.go`

- [ ] **Step 1: Write failing worker tests**

Create `internal/outbox/worker_test.go`:

```go
package outbox

import (
	"context"
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

	// Wait for delivery.
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
		return nil // succeed on second attempt
	}

	q.Enqueue(Item{DestType: "ap", DestURL: "https://a.com", Payload: "{}", MaxAttempts: 3})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wp := NewWorkerPool(q, WorkerPoolConfig{
		APWorkers:    1,
		PollInterval: 50 * time.Millisecond,
	})
	wp.RegisterDeliverer("ap", deliverFn)
	wp.Start(ctx)

	// Wait for second attempt — need to force the retry time to past.
	time.Sleep(200 * time.Millisecond)
	// Force retry now (the backoff for attempt 2 is 5s which is too long for a test).
	rows, _ := q.db.Query(`SELECT id FROM outbox WHERE status = 'pending' AND attempts > 0`)
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		q.forceRetryNow(id)
	}
	rows.Close()

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

	var order []int
	deliverFn := func(ctx context.Context, item Item) error {
		order = append(order, item.Priority)
		return nil
	}

	// Enqueue background first, then real-time.
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

	deadline := time.After(2 * time.Second)
	for len(order) < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out: delivered %d, want 2", len(order))
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if order[0] != PriorityRealTime {
		t.Errorf("first delivery priority = %d, want %d (real-time)", order[0], PriorityRealTime)
	}
	cancel()
}
```

- [ ] **Step 2: Add missing import to worker_test.go**

The `TestWorkerPoolRetriesOnFailure` test uses `fmt.Errorf` — add `"fmt"` to the import block.

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd /home/alex/GitHub/klistr && go test ./internal/outbox/... -run TestWorkerPool`
Expected: FAIL — `WorkerPool` not defined.

- [ ] **Step 4: Implement the WorkerPool**

Create `internal/outbox/worker.go`:

```go
package outbox

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DeliverFunc is the signature for protocol-specific delivery logic.
// It receives the outbox item and returns nil on success or an error on failure.
type DeliverFunc func(ctx context.Context, item Item) error

// WorkerPoolConfig holds tuning parameters for the worker pool.
type WorkerPoolConfig struct {
	APWorkers    int
	RelayWorkers int
	BskyWorkers  int
	PollInterval time.Duration // how often idle workers check for work
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

// Start launches all worker goroutines. Blocks until ctx is cancelled.
func (wp *WorkerPool) Start(ctx context.Context) {
	var wg sync.WaitGroup

	launch := func(destType string, count int) {
		for i := 0; i < count; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				wp.runWorker(ctx, destType, workerID)
			}(i)
		}
	}

	launch("ap", wp.config.APWorkers)
	launch("relay", wp.config.RelayWorkers)
	launch("bsky", wp.config.BskyWorkers)

	// Don't block — let the caller manage lifetime via ctx.
	go func() {
		<-ctx.Done()
		wg.Wait()
	}()
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
			slog.Warn("outbox claim error", "dest_type", destType, "error", err)
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /home/alex/GitHub/klistr && go test ./internal/outbox/... -v -timeout 30s`
Expected: All tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/outbox/worker.go internal/outbox/worker_test.go
git commit -m "outbox: add WorkerPool with per-dest-type drain goroutines"
```

---

## Task 5: Integrate AP Federation with Outbox

**Files:**
- Modify: `internal/ap/federation.go`
- Modify: `internal/nostr/handler.go` (minor — no behavioral change, just verify it still compiles)

- [ ] **Step 1: Define the Enqueuer interface in federation.go**

Add this interface and update the `Federator` struct. The interface is intentionally narrow so the outbox package doesn't need to be imported by the AP package:

In `internal/ap/federation.go`, add after the import block:

```go
// Enqueuer is an optional interface for persisting outbound deliveries.
// When set on the Federator, activities are enqueued for later delivery
// by a worker pool instead of being delivered inline.
type Enqueuer interface {
	EnqueueAP(destURL, payload string, priority int, sourceEventID string) error
}
```

Add this field to the `Federator` struct:

```go
	// Enqueuer, when non-nil, routes deliveries through the outbox queue
	// instead of performing inline HTTP POSTs.
	Enqueuer Enqueuer
```

- [ ] **Step 2: Modify Federate() to use Enqueuer when available**

Replace the delivery goroutine section inside `Federate()` (the `for inbox := range inboxes` loop, lines 78-103) with:

```go
	if f.Enqueuer != nil {
		// Serialize the activity once; each inbox gets the same payload.
		payloadBytes, err := json.Marshal(activity)
		if err != nil {
			slog.Warn("federation: failed to marshal activity", "id", id, "error", err)
			return
		}
		payload := string(payloadBytes)
		for inbox := range inboxes {
			if err := f.Enqueuer.EnqueueAP(inbox, payload, f.priorityFor(activityType), id); err != nil {
				slog.Warn("federation: failed to enqueue", "inbox", inbox, "error", err)
			}
		}
		slog.Debug("federation enqueued",
			"id", id,
			"type", activityType,
			"inboxes", len(inboxes),
		)
		return
	}

	// Fallback: inline delivery (used when outbox is not configured).
	sem := make(chan struct{}, f.concurrency())
	var wg sync.WaitGroup
	var mu sync.Mutex
	var success, failed int

	for inbox := range inboxes {
		sem <- struct{}{}
		wg.Add(1)
		go func(inbox string) {
			defer func() { <-sem; wg.Done() }()
			if err := f.hostLimiter(inbox).Wait(ctx); err != nil {
				slog.Debug("federation rate limit cancelled", "inbox", inbox)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			if err := DeliverActivity(ctx, inbox, activity, f.KeyID, f.PrivateKey); err != nil {
				slog.Warn("federation failed", "inbox", inbox, "error", err)
				mu.Lock()
				failed++
				mu.Unlock()
			} else {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(inbox)
	}
	wg.Wait()

	slog.Debug("federation complete",
		"id", id,
		"type", activityType,
		"success", success,
		"failed", failed,
	)
```

- [ ] **Step 3: Add the priorityFor helper method**

Add to `internal/ap/federation.go`:

```go
// priorityFor returns the outbox priority for an activity type.
// Real-time interactions (replies, likes) get priority 0; posts get 1; updates get 2.
func (f *Federator) priorityFor(activityType string) int {
	switch activityType {
	case "Like", "EmojiReact", "Announce", "Delete", "Follow", "Undo":
		return 0 // real-time
	case "Update":
		return 2 // background
	default:
		return 1 // normal (Create, etc.)
	}
}
```

- [ ] **Step 4: Add `"encoding/json"` to the imports in federation.go**

Ensure `"encoding/json"` is in the import block of `federation.go`.

- [ ] **Step 5: Build to verify compilation**

Run: `cd /home/alex/GitHub/klistr && go build ./...`
Expected: Compiles successfully. The `Enqueuer` field is nil by default, so existing behavior is unchanged.

- [ ] **Step 6: Commit**

```bash
git add internal/ap/federation.go
git commit -m "ap: add optional Enqueuer to Federator for outbox-backed delivery"
```

---

## Task 6: Integrate Relay Publishing with Outbox

**Files:**
- Modify: `internal/nostr/relay.go`

- [ ] **Step 1: Define the relay Enqueuer interface**

Add to `internal/nostr/relay.go`, near the top (after imports):

```go
// PublishEnqueuer is an optional interface for persisting outbound relay
// deliveries via the outbox queue.
type PublishEnqueuer interface {
	EnqueueRelay(destURL, payload string, priority int, sourceEventID string) error
}
```

Add this field to the `Publisher` struct:

```go
	// Enqueuer, when non-nil, routes publish calls through the outbox queue
	// instead of delivering inline.
	Enqueuer PublishEnqueuer
```

- [ ] **Step 2: Modify Publish() to use Enqueuer when available**

In the `Publish()` method, add this block right after the `allRelays` copy (after line 427), before the circuit-breaker filtering:

```go
	if p.Enqueuer != nil {
		eventJSON, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("relay publish: marshal event: %w", err)
		}
		payload := string(eventJSON)
		// Determine priority from event kind.
		priority := 1 // normal
		switch event.Kind {
		case 7, 6, 5: // reaction, repost, delete
			priority = 0 // real-time
		case 0, 10002: // metadata, relay list
			priority = 2 // background
		}
		for _, url := range allRelays {
			if err := p.Enqueuer.EnqueueRelay(url, payload, priority, event.ID); err != nil {
				slog.Warn("relay publish: failed to enqueue", "relay", url, "error", err)
			}
		}
		return nil
	}
```

- [ ] **Step 3: Add `"encoding/json"` to imports in relay.go if not already present**

Check imports and add `"encoding/json"` if missing.

- [ ] **Step 4: Export circuit breaker check for worker use**

Add this method to `Publisher` so the outbox worker can check circuit state:

```go
// IsCircuitOpen returns true if the given relay's circuit breaker is open.
// Used by outbox workers to fast-fail instead of attempting delivery.
func (p *Publisher) IsCircuitOpen(relayURL string) bool {
	return p.getCircuit(relayURL).isOpen()
}

// RecordRelayResult updates the circuit breaker state for a relay after a
// delivery attempt. Used by outbox workers.
func (p *Publisher) RecordRelayResult(relayURL string, err error) {
	cb := p.getCircuit(relayURL)
	if err == nil {
		cb.recordSuccess()
		return
	}
	if isPowRequired(err) {
		cb.openForPoW()
	} else if !isPolicyRejection(err) {
		cb.recordFailure()
	}
}
```

- [ ] **Step 5: Export GetPool for worker use**

The outbox worker needs direct access to the pool for single-relay publish. Add to `internal/nostr/relay.go`:

```go
// GetPool returns the underlying nostr.SimplePool for direct relay operations.
// Used by outbox workers that need to publish to individual relays.
func (p *Publisher) GetPool() *nostr.SimplePool {
	return p.getPool()
}
```

- [ ] **Step 6: Build to verify compilation**

Run: `cd /home/alex/GitHub/klistr && go build ./...`
Expected: Compiles successfully.

- [ ] **Step 7: Commit**

```bash
git add internal/nostr/relay.go
git commit -m "nostr: add optional PublishEnqueuer to Publisher for outbox-backed relay delivery"
```

---

## Task 7: Integrate Bluesky Posting with Outbox

**Files:**
- Modify: `internal/bsky/poster.go`

- [ ] **Step 1: Define the Bluesky Enqueuer interface**

Add to `internal/bsky/poster.go`, after the `PosterStore` interface:

```go
// PosterEnqueuer is an optional interface for persisting outbound Bluesky
// deliveries via the outbox queue.
type PosterEnqueuer interface {
	EnqueueBsky(destURL, payload string, priority int, sourceEventID string) error
}
```

Add this field to the `Poster` struct:

```go
	// Enqueuer, when non-nil, routes Bluesky posts through the outbox queue
	// instead of delivering inline.
	Enqueuer PosterEnqueuer
```

- [ ] **Step 2: Create a bskyPayload type for serialization**

Add to `internal/bsky/poster.go`:

```go
// bskyPayload is the JSON structure stored in the outbox for Bluesky deliveries.
// It contains enough information for the worker to reconstruct the XRPC call.
type bskyPayload struct {
	Kind    int              `json:"kind"`
	EventID string           `json:"event_id"`
	Event   *nostr.Event     `json:"event"`
}
```

- [ ] **Step 3: Modify Handle() to use Enqueuer when available**

Replace the body of `Handle()` with:

```go
func (p *Poster) Handle(ctx context.Context, event *nostr.Event) {
	if p.Enqueuer != nil {
		// Skip if already bridged (idempotency check before enqueue).
		if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
			return
		}
		payload, err := json.Marshal(bskyPayload{
			Kind:    event.Kind,
			EventID: event.ID,
			Event:   event,
		})
		if err != nil {
			slog.Warn("bsky: failed to marshal event for outbox", "id", event.ID, "error", err)
			return
		}
		priority := 1 // normal
		switch event.Kind {
		case 7, 6, 5: // reaction, repost, delete
			priority = 0
		}
		if err := p.Enqueuer.EnqueueBsky(p.Client.PDSURL, string(payload), priority, event.ID); err != nil {
			slog.Warn("bsky: failed to enqueue", "id", event.ID, "error", err)
		}
		return
	}

	switch event.Kind {
	case 1:
		p.handleKind1(ctx, event)
	case 5:
		p.handleKind5(ctx, event)
	case 6:
		p.handleKind6(ctx, event)
	case 7:
		p.handleKind7(ctx, event)
	}
}
```

- [ ] **Step 4: Add a HandleDirect method for worker use**

The outbox worker needs to call the original Handle logic (without re-enqueuing). Add:

```go
// HandleDirect processes a Nostr event with direct Bluesky delivery (no outbox).
// Called by outbox workers to perform the actual XRPC calls.
func (p *Poster) HandleDirect(ctx context.Context, event *nostr.Event) error {
	switch event.Kind {
	case 1:
		p.handleKind1(ctx, event)
	case 5:
		p.handleKind5(ctx, event)
	case 6:
		p.handleKind6(ctx, event)
	case 7:
		p.handleKind7(ctx, event)
	}
	// The individual handlers log errors but don't return them.
	// For the outbox, we treat all Bluesky deliveries as successful unless
	// the handler panics (recovered by the worker pool).
	return nil
}
```

- [ ] **Step 5: Add `"encoding/json"` to imports**

Ensure `"encoding/json"` is in the import block.

- [ ] **Step 6: Build to verify compilation**

Run: `cd /home/alex/GitHub/klistr && go build ./...`
Expected: Compiles successfully.

- [ ] **Step 7: Commit**

```bash
git add internal/bsky/poster.go
git commit -m "bsky: add optional PosterEnqueuer to Poster for outbox-backed delivery"
```

---

## Task 8: Outbox Enqueuer Adapter

**Files:**
- Create: `internal/outbox/enqueuer.go`

This adapter implements all three enqueuer interfaces (AP, Relay, Bluesky) so that `cmd/klistr/main.go` can wire a single object into all three components.

- [ ] **Step 1: Create the adapter**

Create `internal/outbox/enqueuer.go`:

```go
package outbox

// EnqueueAdapter implements the enqueuer interfaces expected by the AP
// Federator, Nostr Publisher, and Bluesky Poster. It wraps a Queue and
// delegates to Enqueue with the appropriate dest_type.
type EnqueueAdapter struct {
	Queue *Queue
}

// EnqueueAP satisfies ap.Enqueuer.
func (e *EnqueueAdapter) EnqueueAP(destURL, payload string, priority int, sourceEventID string) error {
	_, err := e.Queue.Enqueue(Item{
		DestType:      "ap",
		DestURL:       destURL,
		Payload:       payload,
		Priority:      priority,
		SourceEventID: sourceEventID,
	})
	return err
}

// EnqueueRelay satisfies nostr.PublishEnqueuer.
func (e *EnqueueAdapter) EnqueueRelay(destURL, payload string, priority int, sourceEventID string) error {
	_, err := e.Queue.Enqueue(Item{
		DestType:      "relay",
		DestURL:       destURL,
		Payload:       payload,
		Priority:      priority,
		SourceEventID: sourceEventID,
	})
	return err
}

// EnqueueBsky satisfies bsky.PosterEnqueuer.
func (e *EnqueueAdapter) EnqueueBsky(destURL, payload string, priority int, sourceEventID string) error {
	_, err := e.Queue.Enqueue(Item{
		DestType:      "bsky",
		DestURL:       destURL,
		Payload:       payload,
		Priority:      priority,
		SourceEventID: sourceEventID,
	})
	return err
}
```

- [ ] **Step 2: Build to verify**

Run: `cd /home/alex/GitHub/klistr && go build ./...`
Expected: Compiles.

- [ ] **Step 3: Commit**

```bash
git add internal/outbox/enqueuer.go
git commit -m "outbox: add EnqueueAdapter implementing AP/Relay/Bsky enqueuer interfaces"
```

---

## Task 9: Wire Everything in main.go

**Files:**
- Modify: `cmd/klistr/main.go`

- [ ] **Step 1: Add outbox imports**

Add to the import block of `cmd/klistr/main.go`:

```go
	"github.com/klppl/klistr/internal/outbox"
```

- [ ] **Step 2: Create the outbox Queue and WorkerPool after DB setup**

After the `store.Migrate()` call and before the RSA key pair section, add:

```go
	// ─── Outbox Queue ────────────────────────────────────────────────────────
	outboxQueue := outbox.NewQueue(store.DB(), store.Driver())
	enqueuer := &outbox.EnqueueAdapter{Queue: outboxQueue}
```

- [ ] **Step 3: Wire the enqueuer into Federator**

In the Federator initialization (around line 260-269), add the Enqueuer field:

```go
	federator := &ap.Federator{
		LocalDomain: cfg.LocalDomain,
		KeyID:       localActorURL + "#main-key",
		PrivateKey:  keyPair.Private,
		Concurrency: cfg.APFederationConcurrency,
		GetFollowers: func(actorURL string) ([]string, error) {
			return store.GetFollowers(actorURL)
		},
		Enqueuer: enqueuer,
	}
```

- [ ] **Step 4: Wire the enqueuer into Publisher**

After `publisher := nostrpkg.NewPublisher(cfg.NostrRelays)`, add:

```go
	publisher.Enqueuer = enqueuer
```

- [ ] **Step 5: Wire the enqueuer into Bluesky Poster**

In the Bluesky bridge setup section (where `nostrHandler.BskyPoster` is assigned), add the Enqueuer field:

```go
			nostrHandler.BskyPoster = &bsky.Poster{
				Client:          bskyClient,
				Store:           store,
				LocalDomain:     cfg.LocalDomain,
				ExternalBaseURL: cfg.ExternalBaseURL,
				Enqueuer:        enqueuer,
			}
```

- [ ] **Step 6: Create delivery functions and start the WorkerPool**

After the relay pool setup and before the HTTP server, add:

```go
	// ─── Outbox Worker Pool ──────────────────────────────────────────────────
	workerPool := outbox.NewWorkerPool(outboxQueue, outbox.WorkerPoolConfig{
		APWorkers:    cfg.APFederationConcurrency,
		RelayWorkers: 5,
		BskyWorkers:  1,
		PollInterval: 100 * time.Millisecond,
	})

	// AP delivery worker: deserializes payload and calls DeliverActivity.
	workerPool.RegisterDeliverer("ap", func(ctx context.Context, item outbox.Item) error {
		var activity map[string]interface{}
		if err := json.Unmarshal([]byte(item.Payload), &activity); err != nil {
			return fmt.Errorf("unmarshal AP activity: %w", err)
		}
		return ap.DeliverActivity(ctx, item.DestURL, activity, federator.KeyID, keyPair.Private)
	})

	// Relay delivery worker: deserializes event and publishes to one relay.
	workerPool.RegisterDeliverer("relay", func(ctx context.Context, item outbox.Item) error {
		// Fast-fail if circuit is open.
		if publisher.IsCircuitOpen(item.DestURL) {
			return fmt.Errorf("circuit open for %s", item.DestURL)
		}
		var event nostr.Event
		if err := json.Unmarshal([]byte(item.Payload), &event); err != nil {
			return fmt.Errorf("unmarshal nostr event: %w", err)
		}
		publishCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		pool := publisher.GetPool()
		for result := range pool.PublishMany(publishCtx, []string{item.DestURL}, event) {
			publisher.RecordRelayResult(result.RelayURL, result.Error)
			if result.Error != nil {
				return result.Error
			}
		}
		return nil
	})

	// Bluesky delivery worker: deserializes event and calls HandleDirect.
	if activeBskyClient != nil {
		bskyPoster := nostrHandler.BskyPoster.(*bsky.Poster)
		workerPool.RegisterDeliverer("bsky", func(ctx context.Context, item outbox.Item) error {
			var payload struct {
				Kind    int          `json:"kind"`
				EventID string       `json:"event_id"`
				Event   *nostr.Event `json:"event"`
			}
			if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
				return fmt.Errorf("unmarshal bsky payload: %w", err)
			}
			return bskyPoster.HandleDirect(ctx, payload.Event)
		})
	}

	go workerPool.Start(ctx)

	// ─── Outbox Cleanup ──────────────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				removed, err := outboxQueue.Cleanup(48*time.Hour, 168*time.Hour)
				if err != nil {
					slog.Warn("outbox cleanup error", "error", err)
				} else if removed > 0 {
					slog.Info("outbox cleanup", "removed", removed)
				}
			}
		}
	}()
```

- [ ] **Step 7: Add required imports to main.go**

Ensure these are in the import block:
```go
	"encoding/json"
	"time"
```

Also ensure `nostr "github.com/nbd-wtf/go-nostr"` is imported (it likely already is via the existing go-nostr usage).

- [ ] **Step 8: Build to verify compilation**

Run: `cd /home/alex/GitHub/klistr && go build ./cmd/klistr`
Expected: Compiles successfully.

- [ ] **Step 9: Commit**

```bash
git add cmd/klistr/main.go internal/nostr/relay.go
git commit -m "wire outbox queue and worker pool into main startup sequence"
```

---

## Task 10: Admin UI — Outbox Stats and Dead Letter Endpoints

**Files:**
- Modify: `internal/server/admin.go`

- [ ] **Step 1: Add OutboxQueue field to Server**

In `internal/server/server.go` (or wherever the `Server` struct is defined), check for the struct definition and add:

```go
	// OutboxQueue provides outbox stats and dead-letter management for the admin UI.
	OutboxQueue *outbox.Queue
```

Add the import for `"github.com/klppl/klistr/internal/outbox"`.

- [ ] **Step 2: Add outbox API routes**

In `internal/server/admin.go`, find where admin routes are mounted (look for the route group that handles `/web/api/`). Add these routes:

```go
	r.Get("/web/api/outbox/stats", s.handleOutboxStats)
	r.Get("/web/api/outbox/dead", s.handleOutboxDeadLetters)
	r.Post("/web/api/outbox/retry", s.handleOutboxRetry)
```

- [ ] **Step 3: Implement the three handlers**

Add to `internal/server/admin.go`:

```go
func (s *Server) handleOutboxStats(w http.ResponseWriter, r *http.Request) {
	if s.OutboxQueue == nil {
		http.Error(w, "outbox not configured", http.StatusServiceUnavailable)
		return
	}
	stats, err := s.OutboxQueue.Stats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleOutboxDeadLetters(w http.ResponseWriter, r *http.Request) {
	if s.OutboxQueue == nil {
		http.Error(w, "outbox not configured", http.StatusServiceUnavailable)
		return
	}
	items, err := s.OutboxQueue.DeadLetters()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []outbox.Item{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

func (s *Server) handleOutboxRetry(w http.ResponseWriter, r *http.Request) {
	if s.OutboxQueue == nil {
		http.Error(w, "outbox not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := s.OutboxQueue.RetryDead(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
```

- [ ] **Step 4: Wire OutboxQueue in main.go**

In `cmd/klistr/main.go`, where the HTTP server is created, pass the outbox queue:

```go
	srv.OutboxQueue = outboxQueue
```

(Find the section where `srv` fields like `SetBskyClient`, `SetShowSourceLink` etc. are called.)

- [ ] **Step 5: Build to verify**

Run: `cd /home/alex/GitHub/klistr && go build ./cmd/klistr`
Expected: Compiles.

- [ ] **Step 6: Commit**

```bash
git add internal/server/admin.go internal/server/server.go cmd/klistr/main.go
git commit -m "admin: add outbox stats, dead-letter list, and manual retry endpoints"
```

---

## Task 11: Integration Smoke Test

**Files:**
- Create: `internal/outbox/integration_test.go`

This test verifies the full flow: Enqueue → Worker → Delivery → Complete.

- [ ] **Step 1: Write the integration test**

Create `internal/outbox/integration_test.go`:

```go
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

	// Track deliveries per dest_type.
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

	// Enqueue mixed items.
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

	// Wait for all deliveries.
	deadline := time.After(4 * time.Second)
	for apCount.Load() < 5 || relayCount.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("timed out: ap=%d relay=%d", apCount.Load(), relayCount.Load())
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Verify stats show all done.
	time.Sleep(100 * time.Millisecond) // let Complete() calls finish
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
		MaxAttempts: 1, // dead after first failure
	})

	// Wait for dead letter.
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

	// Verify dead letter is retrievable.
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
```

- [ ] **Step 2: Run all tests**

Run: `cd /home/alex/GitHub/klistr && go test ./internal/outbox/... -v -timeout 30s`
Expected: All tests PASS.

- [ ] **Step 3: Run full project build**

Run: `cd /home/alex/GitHub/klistr && go build ./cmd/klistr && go vet ./...`
Expected: Clean build, no vet warnings.

- [ ] **Step 4: Commit**

```bash
git add internal/outbox/integration_test.go
git commit -m "outbox: add integration tests for full delivery and dead-letter flows"
```

---

## Task 12: Update CLAUDE.md

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Add outbox documentation to CLAUDE.md**

In the Architecture section, after the Data Flow diagram, add:

```markdown
### Outbox Queue (At-Least-Once Delivery)

All outbound deliveries (AP federation, Nostr relay publish, Bluesky XRPC) flow through a persistent outbox queue backed by the same SQLite/PostgreSQL database. The `outbox` table stores serialized payloads with priority lanes and exponential backoff retry.

```
Event → Handler → outbox.Enqueue(dest_type, dest_url, payload, priority)
                   ↓ (returns immediately)
Worker Pool → Claim → Deliver → Complete | Fail+Retry | Dead-Letter
```

**Priority lanes:** 0=real-time (replies, likes, follows), 1=normal (posts), 2=background (resyncs).
**Backoff:** immediate → 5s → 30s → 2m → 10m → 1h → dead letter.
**Workers:** AP (default 10), Relay (default 5), Bluesky (1).
**Admin UI:** `/web/api/outbox/stats`, `/web/api/outbox/dead`, `POST /web/api/outbox/retry`.
```

In the Package Overview, add:

```markdown
- **`internal/outbox/`** — Persistent delivery queue with at-least-once semantics. `Queue` manages the `outbox` table (enqueue, claim, complete, fail, dead-letter, cleanup). `WorkerPool` runs per-dest-type goroutines that drain the queue. `EnqueueAdapter` implements the enqueuer interfaces for AP/Relay/Bluesky integration.
```

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: add outbox queue documentation to CLAUDE.md"
```
