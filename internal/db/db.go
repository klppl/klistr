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
		// SQLite performs best with WAL mode and a single writer.
		db.SetMaxOpenConns(1)
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			return nil, fmt.Errorf("enable WAL: %w", err)
		}
		if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
			return nil, fmt.Errorf("enable foreign_keys: %w", err)
		}
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

func (s *Store) migrateSQLite() error {
	migrations := []string{
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
	}

	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migration failed: %w\nSQL: %s", err, m)
		}
	}
	slog.Info("migrations complete")
	return nil
}

func (s *Store) migratePostgres() error {
	migrations := []string{
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
	}

	for _, m := range migrations {
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

// GetFollowing returns all followed IDs for a given follower ID.
func (s *Store) GetFollowing(followerID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT followed_id FROM follows WHERE follower_id = `+s.ph(), followerID)
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

// ─── Helpers ──────────────────────────────────────────────────────────────────

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
