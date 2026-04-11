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

// Reschedule puts a claimed item back to pending with a specific delay,
// without incrementing the attempt counter. Used for circuit-open fast-fail.
func (q *Queue) Reschedule(id int64, delay time.Duration) error {
	nextRetry := time.Now().UTC().Add(delay).Format(time.RFC3339Nano)
	var query string
	if q.driver == "sqlite" {
		query = `UPDATE outbox SET status = 'pending', next_retry_at = ? WHERE id = ?`
	} else {
		query = `UPDATE outbox SET status = 'pending', next_retry_at = $1 WHERE id = $2`
	}
	_, err := q.db.Exec(query, nextRetry, id)
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
