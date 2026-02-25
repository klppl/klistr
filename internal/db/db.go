// Package db handles database connectivity, migrations, and data access
// for the klistr bridge. It supports both SQLite (default, no external
// dependencies) and PostgreSQL (for larger deployments).
package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// Store wraps a database connection and provides all data access methods.
type Store struct {
	db     *sql.DB
	driver string

	// In-memory caches to reduce DB round-trips.
	objectsByAP    sync.Map // ap_id → nostr_id
	objectsByNostr sync.Map // nostr_id → ap_id
}

// Open opens a database connection. The URL can be:
//   - A file path like "klistr.db" → SQLite
//   - "sqlite:///path/to/file.db" → SQLite
//   - "postgres://..." → PostgreSQL
func Open(databaseURL string) (*Store, error) {
	driver, dsn := detectDriver(databaseURL)

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	if driver == "sqlite" {
		// WAL mode allows multiple concurrent readers alongside one writer.
		// We allow a small connection pool so read-heavy operations (cache
		// misses, stats, follower queries) can proceed in parallel instead of
		// all queuing behind every write.  SQLite serialises writers itself;
		// busy_timeout makes that serialisation graceful (retry for up to 5s)
		// rather than immediately returning SQLITE_BUSY to the caller.
		//
		// For deployments receiving >~50 concurrent inbox activities, switch to
		// PostgreSQL (already supported via DATABASE_URL=postgres://...) —
		// SQLite's single-writer architecture is a hard ceiling that no tuning
		// can fully remove.
		const sqliteMaxConns = 4
		db.SetMaxOpenConns(sqliteMaxConns)
		db.SetMaxIdleConns(sqliteMaxConns)

		for _, pragma := range []string{
			"PRAGMA journal_mode=WAL",
			"PRAGMA busy_timeout=5000", // ms; retries writes instead of SQLITE_BUSY
			"PRAGMA foreign_keys=ON",
			"PRAGMA synchronous=NORMAL", // safe with WAL; faster than FULL
		} {
			if _, err := db.Exec(pragma); err != nil {
				return nil, fmt.Errorf("sqlite pragma (%s): %w", pragma, err)
			}
		}

		slog.Info("sqlite database opened",
			"max_conns", sqliteMaxConns,
			"note", "switch to PostgreSQL for high-traffic deployments",
		)
	}

	return &Store{db: db, driver: driver}, nil
}

// Migrate runs all pending database migrations.
func (s *Store) Migrate() error {
	slog.Info("running database migrations")

	if s.driver == "sqlite" {
		return s.migrateSQLite()
	}
	return s.migratePostgres()
}

// commonMigrations lists DDL statements shared between SQLite and PostgreSQL.
// Any new migration must be appended here; driver-specific error handling is
// applied by migrateSQLite / migratePostgres.
var commonMigrations = []string{
	`CREATE TABLE IF NOT EXISTS objects (
		ap_id    TEXT NOT NULL UNIQUE,
		nostr_id TEXT NOT NULL UNIQUE
	)`,
	`CREATE INDEX IF NOT EXISTS objects_nostr_id ON objects(nostr_id)`,
	`CREATE TABLE IF NOT EXISTS follows (
		follower_id TEXT NOT NULL,
		followed_id TEXT NOT NULL,
		UNIQUE(follower_id, followed_id)
	)`,
	`CREATE INDEX IF NOT EXISTS follows_follower ON follows(follower_id)`,
	`CREATE INDEX IF NOT EXISTS follows_followed ON follows(followed_id)`,
	`CREATE TABLE IF NOT EXISTS actor_keys (
		pubkey       TEXT NOT NULL PRIMARY KEY,
		ap_actor_url TEXT NOT NULL UNIQUE
	)`,
	`CREATE TABLE IF NOT EXISTS kv (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	// Indexes covering the LIKE-prefix filters used by Stats() and type-filtered
	// follower/following queries. Prefix scans are efficient once the column is indexed.
	`CREATE INDEX IF NOT EXISTS objects_ap_id ON objects(ap_id)`,
	`CREATE INDEX IF NOT EXISTS follows_follower_type ON follows(followed_id, follower_id)`,
	// Append-only admin audit log.  ts is an RFC3339Nano timestamp; ISO 8601
	// lexicographic ordering lets both SQLite and PostgreSQL sort by ts DESC.
	`CREATE TABLE IF NOT EXISTS audit_log (
		ts     TEXT NOT NULL,
		action TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS audit_log_ts ON audit_log(ts)`,
}

func (s *Store) migrateSQLite() error {
	for _, m := range commonMigrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migration failed: %w\nSQL: %s", err, m)
		}
	}
	slog.Info("migrations complete")
	return nil
}

func (s *Store) migratePostgres() error {
	for _, m := range commonMigrations {
		if _, err := s.db.Exec(m); err != nil {
			// Ignore "already exists" errors on index creation for idempotency.
			if strings.Contains(err.Error(), "already exists") {
				continue
			}
			return fmt.Errorf("migration failed: %w", err)
		}
	}
	slog.Info("migrations complete")
	return nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// ─── Objects ──────────────────────────────────────────────────────────────────

// GetAPIDForObject returns the ActivityPub ID for a Nostr event ID, if known.
func (s *Store) GetAPIDForObject(nostrID string) (string, bool) {
	if v, ok := s.objectsByNostr.Load(nostrID); ok {
		return v.(string), true
	}
	var apID string
	err := s.db.QueryRow(`SELECT ap_id FROM objects WHERE nostr_id = `+s.ph(), nostrID).Scan(&apID)
	if err != nil {
		return "", false
	}
	s.objectsByNostr.Store(nostrID, apID)
	s.objectsByAP.Store(apID, nostrID)
	return apID, true
}

// GetNostrIDForObject returns the Nostr event ID for an ActivityPub object ID, if known.
func (s *Store) GetNostrIDForObject(apID string) (string, bool) {
	if v, ok := s.objectsByAP.Load(apID); ok {
		return v.(string), true
	}
	var nostrID string
	err := s.db.QueryRow(`SELECT nostr_id FROM objects WHERE ap_id = `+s.ph(), apID).Scan(&nostrID)
	if err != nil {
		return "", false
	}
	s.objectsByNostr.Store(nostrID, apID)
	s.objectsByAP.Store(apID, nostrID)
	return nostrID, true
}

// DeleteObject removes an ActivityPub ↔ Nostr object ID mapping from the
// database and evicts both cache entries. Called when a Delete activity or a
// kind-5 deletion event is processed so that stale mappings cannot cause ghost
// re-deliveries or false-positive idempotency hits.
func (s *Store) DeleteObject(apID, nostrID string) error {
	var q string
	if s.driver == "sqlite" {
		q = `DELETE FROM objects WHERE ap_id = ? AND nostr_id = ?`
	} else {
		q = `DELETE FROM objects WHERE ap_id = $1 AND nostr_id = $2`
	}
	_, err := s.db.Exec(q, apID, nostrID)
	// Evict from both caches regardless of whether a DB row was found.
	s.objectsByAP.Delete(apID)
	s.objectsByNostr.Delete(nostrID)
	return err
}

// AddObject stores an ActivityPub ↔ Nostr object ID mapping.
func (s *Store) AddObject(apID, nostrID string) error {
	var q string
	if s.driver == "sqlite" {
		q = `INSERT OR IGNORE INTO objects (ap_id, nostr_id) VALUES (?, ?)`
	} else {
		q = `INSERT INTO objects (ap_id, nostr_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`
	}
	_, err := s.db.Exec(q, apID, nostrID)
	if err == nil {
		s.objectsByNostr.Store(nostrID, apID)
		s.objectsByAP.Store(apID, nostrID)
	}
	return err
}

// ─── Follows ──────────────────────────────────────────────────────────────────

// AddFollow records that followerID follows followedID.
func (s *Store) AddFollow(followerID, followedID string) error {
	var q string
	if s.driver == "sqlite" {
		q = `INSERT OR IGNORE INTO follows (follower_id, followed_id) VALUES (?, ?)`
	} else {
		q = `INSERT INTO follows (follower_id, followed_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`
	}
	_, err := s.db.Exec(q, followerID, followedID)
	return err
}

// RemoveFollow removes a follow relationship.
func (s *Store) RemoveFollow(followerID, followedID string) error {
	var q string
	if s.driver == "sqlite" {
		q = `DELETE FROM follows WHERE follower_id = ? AND followed_id = ?`
	} else {
		q = `DELETE FROM follows WHERE follower_id = $1 AND followed_id = $2`
	}
	_, err := s.db.Exec(q, followerID, followedID)
	return err
}

// GetFollowers returns all follower IDs for a given followed ID.
func (s *Store) GetFollowers(followedID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT follower_id FROM follows WHERE followed_id = `+s.ph(), followedID)
	if err != nil {
		return nil, err
	}
	return scanStringRows(rows)
}

// GetAPFollowers returns only ActivityPub follower IDs (those starting with "http")
// for a given followed ID. Bluesky entries (prefixed "bsky:") are excluded.
func (s *Store) GetAPFollowers(followedID string) ([]string, error) {
	var q string
	if s.driver == "sqlite" {
		q = `SELECT follower_id FROM follows WHERE followed_id = ? AND follower_id LIKE 'http%'`
	} else {
		q = `SELECT follower_id FROM follows WHERE followed_id = $1 AND follower_id LIKE 'http%'`
	}
	rows, err := s.db.Query(q, followedID)
	if err != nil {
		return nil, err
	}
	return scanStringRows(rows)
}

// GetBskyFollowers returns only Bluesky follower IDs (those starting with "bsky:")
// for a given followed ID.
func (s *Store) GetBskyFollowers(followedID string) ([]string, error) {
	var q string
	if s.driver == "sqlite" {
		q = `SELECT follower_id FROM follows WHERE followed_id = ? AND follower_id LIKE 'bsky:%'`
	} else {
		q = `SELECT follower_id FROM follows WHERE followed_id = $1 AND follower_id LIKE 'bsky:%'`
	}
	rows, err := s.db.Query(q, followedID)
	if err != nil {
		return nil, err
	}
	return scanStringRows(rows)
}

// GetFollowing returns all followed IDs for a given follower ID.
func (s *Store) GetFollowing(followerID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT followed_id FROM follows WHERE follower_id = `+s.ph(), followerID)
	if err != nil {
		return nil, err
	}
	return scanStringRows(rows)
}

// GetAPFollowing returns only ActivityPub followed IDs (those starting with "http")
// for a given follower ID. Bluesky entries (prefixed "bsky:") are excluded.
func (s *Store) GetAPFollowing(followerID string) ([]string, error) {
	var q string
	if s.driver == "sqlite" {
		q = `SELECT followed_id FROM follows WHERE follower_id = ? AND followed_id LIKE 'http%'`
	} else {
		q = `SELECT followed_id FROM follows WHERE follower_id = $1 AND followed_id LIKE 'http%'`
	}
	rows, err := s.db.Query(q, followerID)
	if err != nil {
		return nil, err
	}
	return scanStringRows(rows)
}

// GetBskyFollowing returns only Bluesky followed IDs (those starting with "bsky:")
// for a given follower ID.
func (s *Store) GetBskyFollowing(followerID string) ([]string, error) {
	var q string
	if s.driver == "sqlite" {
		q = `SELECT followed_id FROM follows WHERE follower_id = ? AND followed_id LIKE 'bsky:%'`
	} else {
		q = `SELECT followed_id FROM follows WHERE follower_id = $1 AND followed_id LIKE 'bsky:%'`
	}
	rows, err := s.db.Query(q, followerID)
	if err != nil {
		return nil, err
	}
	return scanStringRows(rows)
}

// ─── Actor keys ───────────────────────────────────────────────────────────────

// StoreActorKey persists a derived Nostr pubkey → AP actor URL mapping.
func (s *Store) StoreActorKey(pubkey, apActorURL string) error {
	var q string
	if s.driver == "sqlite" {
		q = `INSERT OR IGNORE INTO actor_keys (pubkey, ap_actor_url) VALUES (?, ?)`
	} else {
		q = `INSERT INTO actor_keys (pubkey, ap_actor_url) VALUES ($1, $2) ON CONFLICT DO NOTHING`
	}
	_, err := s.db.Exec(q, pubkey, apActorURL)
	return err
}

// GetAllActorURLs returns every AP actor URL stored in actor_keys.
// Used by AccountResyncer to periodically re-fetch and refresh profile data.
func (s *Store) GetAllActorURLs() ([]string, error) {
	rows, err := s.db.Query(`SELECT ap_actor_url FROM actor_keys`)
	if err != nil {
		return nil, err
	}
	return scanStringRows(rows)
}

// GetActorForKey returns the AP actor URL for a derived Nostr pubkey, if known.
func (s *Store) GetActorForKey(pubkey string) (string, bool) {
	var apActorURL string
	err := s.db.QueryRow(`SELECT ap_actor_url FROM actor_keys WHERE pubkey = `+s.ph(), pubkey).Scan(&apActorURL)
	if err != nil {
		return "", false
	}
	return apActorURL, true
}

// ─── Key-Value store ──────────────────────────────────────────────────────────

// SetKV upserts a key-value pair. Used for persistent state like polling cursors.
func (s *Store) SetKV(key, value string) error {
	var q string
	if s.driver == "sqlite" {
		q = `INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`
	} else {
		q = `INSERT INTO kv (key, value) VALUES ($1, $2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`
	}
	_, err := s.db.Exec(q, key, value)
	return err
}

// GetKV retrieves a value by key. Returns ("", false) if not found.
func (s *Store) GetKV(key string) (string, bool) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key = `+s.ph(), key).Scan(&value)
	if err != nil {
		return "", false
	}
	return value, true
}

// ─── Audit log ────────────────────────────────────────────────────────────────

// AuditLogEntry is one record in the admin audit log.
type AuditLogEntry struct {
	Timestamp string `json:"ts"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
}

// WriteAuditLog appends a new entry to the audit log. It is a best-effort
// call — the caller should log but not propagate any error.
func (s *Store) WriteAuditLog(action, detail string) error {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	var q string
	if s.driver == "sqlite" {
		q = `INSERT INTO audit_log (ts, action, detail) VALUES (?, ?, ?)`
	} else {
		q = `INSERT INTO audit_log (ts, action, detail) VALUES ($1, $2, $3)`
	}
	_, err := s.db.Exec(q, ts, action, detail)
	return err
}

// GetAuditLog returns up to limit entries from the audit log, newest first.
func (s *Store) GetAuditLog(limit int) ([]AuditLogEntry, error) {
	var q string
	if s.driver == "sqlite" {
		q = `SELECT ts, action, detail FROM audit_log ORDER BY ts DESC LIMIT ?`
	} else {
		q = `SELECT ts, action, detail FROM audit_log ORDER BY ts DESC LIMIT $1`
	}
	rows, err := s.db.Query(q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []AuditLogEntry
	for rows.Next() {
		var e AuditLogEntry
		if err := rows.Scan(&e.Timestamp, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ─── Stats ────────────────────────────────────────────────────────────────────

// StoreStats holds aggregate counts returned by Stats.
type StoreStats struct {
	// Fediverse bridge
	FollowerCount    int // AP followers only (follower_id LIKE 'http%')
	ActorKeyCount    int
	FediverseObjects int // objects whose ap_id starts with http (AP URLs)
	// Bluesky bridge
	BskyFollowerCount int    // Bluesky followers (follower_id LIKE 'bsky:%')
	BskyObjects       int    // objects whose ap_id starts with at:// (AT Protocol URIs)
	BskyLastSeen      string // ISO 8601 timestamp of last processed notification; empty if none
	BskyLastPoll      string // ISO 8601 timestamp of last successful API poll; empty if never run
	// Combined
	TotalObjects int
	// Account resync
	LastResyncAt    string // ISO 8601 timestamp of last profile resync; empty if never run
	LastResyncCount string // e.g. "42/43" (ok/total) from last resync
}

// Stats returns aggregate counts for the given followed actor URL.
// The 9 original single-column queries are reduced to 2 batched SQL statements
// plus 3 KV lookups, using FILTER (ANSI SQL, supported by SQLite ≥ 3.30 and PostgreSQL).
func (s *Store) Stats(followedID string) (StoreStats, error) {
	var st StoreStats

	// ── Query 1: follower counts split by bridge type ──────────────────────────
	var followersQ string
	if s.driver == "sqlite" {
		followersQ = `
			SELECT
				COUNT(*) FILTER (WHERE follower_id LIKE 'http%') AS ap_followers,
				COUNT(*) FILTER (WHERE follower_id LIKE 'bsky:%') AS bsky_followers
			FROM follows
			WHERE followed_id = ?`
	} else {
		followersQ = `
			SELECT
				COUNT(*) FILTER (WHERE follower_id LIKE 'http%') AS ap_followers,
				COUNT(*) FILTER (WHERE follower_id LIKE 'bsky:%') AS bsky_followers
			FROM follows
			WHERE followed_id = $1`
	}
	if err := s.db.QueryRow(followersQ, followedID).Scan(&st.FollowerCount, &st.BskyFollowerCount); err != nil {
		return st, err
	}

	// ── Query 2: object and actor-key counts ──────────────────────────────────
	// CTE combines counts across two tables in a single roundtrip.
	const objectsQ = `
		WITH obj AS (
			SELECT
				COUNT(*) FILTER (WHERE ap_id LIKE 'http%')  AS fediverse_objects,
				COUNT(*) FILTER (WHERE ap_id LIKE 'at://%') AS bsky_objects,
				COUNT(*)                                     AS total_objects
			FROM objects
		), ak AS (
			SELECT COUNT(*) AS actor_key_count FROM actor_keys
		)
		SELECT fediverse_objects, bsky_objects, total_objects, actor_key_count
		FROM obj, ak`

	if err := s.db.QueryRow(objectsQ).Scan(
		&st.FediverseObjects, &st.BskyObjects, &st.TotalObjects, &st.ActorKeyCount,
	); err != nil {
		return st, err
	}

	// ── KV lookups: timestamps and resync metadata ─────────────────────────────
	st.BskyLastSeen, _ = s.GetKV("bsky_last_seen_at")
	st.BskyLastPoll, _ = s.GetKV("bsky_last_poll_at")
	st.LastResyncAt, _ = s.GetKV("last_resync_at")
	st.LastResyncCount, _ = s.GetKV("last_resync_count")
	return st, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// scanStringRows scans a single-string-column result set into a slice.
// It closes rows before returning.
func scanStringRows(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var result []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// GetLocalObjectCount returns the number of locally-originated AP objects
// (i.e. ap_id values that begin with the given URL prefix).
func (s *Store) GetLocalObjectCount(prefix string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM objects WHERE ap_id LIKE `+s.ph(),
		prefix+"%",
	).Scan(&n)
	return n, err
}

// GetRecentLocalObjects returns up to limit ap_id values whose ap_id starts with
// prefix. The order is unspecified but consistent within a single DB instance.
func (s *Store) GetRecentLocalObjects(prefix string, limit int) ([]string, error) {
	var q string
	if s.driver == "sqlite" {
		q = `SELECT ap_id FROM objects WHERE ap_id LIKE ? ORDER BY rowid DESC LIMIT ?`
	} else {
		q = `SELECT ap_id FROM objects WHERE ap_id LIKE $1 ORDER BY ap_id DESC LIMIT $2`
	}
	rows, err := s.db.Query(q, prefix+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

// ph returns the SQL placeholder token for a single-argument query.
// SQLite uses ? and PostgreSQL uses $1.
func (s *Store) ph() string {
	if s.driver == "postgres" {
		return "$1"
	}
	return "?"
}

func detectDriver(u string) (driver, dsn string) {
	if strings.HasPrefix(u, "postgres://") || strings.HasPrefix(u, "postgresql://") {
		return "postgres", u
	}
	if strings.HasPrefix(u, "sqlite://") {
		return "sqlite", strings.TrimPrefix(u, "sqlite://")
	}
	// Treat bare paths as SQLite file paths.
	return "sqlite", u
}
