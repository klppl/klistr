package db

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// openMemStore opens an in-memory SQLite store for testing. MaxOpenConns(1)
// is required because ":memory:" creates a separate DB per connection.
func openMemStore(t *testing.T) *Store {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	return &Store{db: conn, driver: "sqlite"}
}

func TestMigrate_FreshDB(t *testing.T) {
	s := openMemStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("first migrate failed: %v", err)
	}
	// Idempotency: second migrate must not error.
	if err := s.Migrate(); err != nil {
		t.Fatalf("second migrate failed (not idempotent): %v", err)
	}
}

// TestMigrate_OldOutboxSchema_GetsClaimedAtAndDedupIndex simulates a
// pre-existing deployment whose outbox table predates the claimed_at column
// and the partial unique dedup index — exactly the production state that
// triggered: "no such column: claimed_at" and "ON CONFLICT clause does not
// match any PRIMARY KEY or UNIQUE constraint". Migrate must upgrade them.
func TestMigrate_OldOutboxSchema_GetsClaimedAtAndDedupIndex(t *testing.T) {
	s := openMemStore(t)

	// Plant the old schema first: no claimed_at, no outbox_dedup index.
	if _, err := s.db.Exec(`CREATE TABLE outbox (
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
	)`); err != nil {
		t.Fatal(err)
	}

	// Run migrations. Must succeed and add the missing pieces.
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate on legacy schema failed: %v", err)
	}

	// 1. claimed_at column must now exist (the reaper depends on it).
	var dummy any
	if err := s.db.QueryRow(`SELECT claimed_at FROM outbox LIMIT 1`).Scan(&dummy); err != nil && err != sql.ErrNoRows {
		t.Errorf("claimed_at column missing after Migrate: %v", err)
	}

	// 2. Partial unique index must now exist (the dedup INSERT...ON CONFLICT
	//    needs it). Verify by inserting one row twice with same
	//    (dest_type, dest_url, source_event_id) and confirming the second
	//    insert is a no-op via ON CONFLICT.
	if _, err := s.db.Exec(`INSERT INTO outbox (dest_type, dest_url, payload, next_retry_at, created_at, source_event_id)
		VALUES ('ap', 'https://x/inbox', '{}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'evt-1')`); err != nil {
		t.Fatalf("first insert failed: %v", err)
	}
	// This second insert is the production query shape from outbox.Enqueue.
	res, err := s.db.Exec(`INSERT INTO outbox (dest_type, dest_url, payload, next_retry_at, created_at, source_event_id)
		VALUES ('ap', 'https://x/inbox', '{"dup":true}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'evt-1')
		ON CONFLICT(dest_type, dest_url, source_event_id)
		WHERE source_event_id IS NOT NULL AND source_event_id != ''
		DO NOTHING`)
	if err != nil {
		t.Fatalf("ON CONFLICT INSERT failed — partial unique index missing: %v", err)
	}
	rows, _ := res.RowsAffected()
	if rows != 0 {
		t.Errorf("duplicate insert should be deduped (0 rows), got %d rows", rows)
	}
}

// TestMigrate_PreExistingDuplicates simulates the unlucky case where the
// legacy DB already contains rows that would violate the new partial unique
// index. The pre-clean DELETE in postMigrations must collapse them so the
// index can be installed; without that step, CREATE UNIQUE INDEX would fail
// and crash startup.
func TestMigrate_PreExistingDuplicates(t *testing.T) {
	s := openMemStore(t)

	if _, err := s.db.Exec(`CREATE TABLE outbox (
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
	)`); err != nil {
		t.Fatal(err)
	}

	// Two duplicate rows for the same (dest_type, dest_url, source_event_id).
	for i := 0; i < 2; i++ {
		if _, err := s.db.Exec(`INSERT INTO outbox (dest_type, dest_url, payload, next_retry_at, created_at, source_event_id)
			VALUES ('ap', 'https://x/inbox', '{}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'evt-1')`); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate must collapse duplicates and install index: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE source_event_id = 'evt-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 row after dedup sweep, got %d", count)
	}
}
