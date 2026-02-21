# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./cmd/klistr          # Build binary
go test ./...                  # Run all tests
go test ./internal/ap/...      # Run tests in a specific package
go build -ldflags="-w -s" ./cmd/klistr  # Production build (smaller binary)
docker compose up -d           # Run with Docker
```

Required environment before running:
```bash
export NOSTR_PRIVATE_KEY=<your hex private key>
export NOSTR_USERNAME=alice
export LOCAL_DOMAIN=https://your-domain.com
./klistr
```

## Architecture

klistr is a single-user personal bridge between Nostr and ActivityPub (Fediverse). It runs as a single binary, acting as an **ActivityPub server** and a **Nostr client** simultaneously — bridging one Nostr identity to one AP actor.

### Data Flow

```
AP (Mastodon etc.)  →  POST /inbox  →  ap.APHandler  →  Nostr relay (publish event)
Nostr relay (author filter)  →  nostr.Handler  →  ap.Federator  →  AP inboxes (HTTP POST)
```

### Package Overview

- **`cmd/klistr/`** — Entry point. Wires all components together: loads config, opens DB, initializes RSA keys, creates handlers, starts relay pool and HTTP server.
- **`internal/config/`** — Environment variable configuration. `config.Load()` exits on missing `NOSTR_PRIVATE_KEY`. Derives `NostrPublicKey` automatically. `NostrUsername` defaults to first 8 chars of pubkey.
- **`internal/db/`** — Database layer (`db.Store`). Supports SQLite (default, WAL mode) and PostgreSQL. Stores two tables: `objects` (AP↔Nostr event ID), `follows`. Uses `sync.Map` caches to reduce DB round-trips. SQL queries differ by driver (`?` vs `$1`).
- **`internal/ap/`** — ActivityPub logic:
  - `transmute.go` — Converts Nostr events → AP objects (`ToActor`, `ToNote`, `ToAnnounce`, `ToLike`, etc.) and builds AP activities (`BuildCreate`, `BuildUpdate`, `BuildFollow`, `BuildAccept`). Uses `TransmuteContext.LocalActorURL` (single actor URL).
  - `handler.go` — `APHandler`: receives incoming AP activities from HTTP inbox and converts them to Nostr events, then publishes to relays. Also fetches/caches AP actors and objects. Detects local actor to sign with real key.
  - `federation.go` — `Federator`: delivers AP activities outbound to remote inboxes. Resolves follower lists (keyed by full actor URL), fetches actor inboxes, deduplicates by shared inbox per origin.
  - `client.go` — HTTP client for fetching AP actors and objects from remote servers, with in-memory caching.
  - `crypto.go` / `keys.go` — RSA key management; auto-generates key pair if PEM files don't exist.
  - `types.go` — AP type definitions (Actor, Note, Activity, etc.).
- **`internal/nostr/`** — Nostr protocol handling:
  - `signer.go` — `Signer`: provides dual signing — `SignAsUser` uses the real private key; `Sign(event, apID)` derives a deterministic key via `SHA-256(localPrivKey + ":" + apActorID)`.
  - `relay.go` — `RelayPool`: subscribes to a single author's events (kinds 0,1,3,5,6,7,9735) from configured relays. `Publisher`: publishes events to write relays.
  - `handler.go` — `Handler`: processes Nostr events. Eligibility: skip events with a `proxy` tag (loop prevention). No DB lookups needed since the subscription is already author-filtered.
- **`internal/server/`** — Chi-based HTTP server. Serves WebFinger, NodeInfo, host-meta, NIP-05, AP actor/object/inbox/outbox/followers/following endpoints. Returns 404 for any username that isn't the configured `NostrUsername`.

### Identity

- **Local AP actor URL**: `https://LOCAL_DOMAIN/users/<NostrUsername>`
- **Nostr event → AP URL**: `https://LOCAL_DOMAIN/objects/<event-id>`
- **AP actor → Nostr keypair**: deterministic derivation via `Signer` (seed = `NostrPrivateKey + ":" + apActorID`)

### Loop Prevention

Events tagged with `["proxy", ..., "activitypub"]` are bridged events and skipped by the Nostr handler. The relay subscription is author-filtered so only the local user's events are received.

### Database

- SQLite path detection: bare filename → SQLite, `postgres://` prefix → PostgreSQL
- SQLite is single-writer (`SetMaxOpenConns(1)`) with WAL mode
- Migrations are idempotent (`CREATE TABLE IF NOT EXISTS`, `INSERT OR IGNORE`)
- Follows are stored with full AP actor URLs as both `follower_id` and `followed_id`

## Module

```
module github.com/klppl/klistr
go 1.23.1
```

Key dependencies: `go-chi/chi` (HTTP router), `nbd-wtf/go-nostr` (Nostr protocol), `modernc.org/sqlite` (pure-Go SQLite driver), `go-fed/httpsig` (HTTP Signatures for AP).
