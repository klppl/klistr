# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./cmd/klistr          # Build binary
go test ./...                  # Run all tests (no tests currently exist)
go test ./internal/ap/...      # Run tests in a specific package
go build -ldflags="-w -s" ./cmd/klistr  # Production build (smaller binary)
docker compose up -d           # Run with Docker
```

Required environment before running:
```bash
export NOSTR_PRIVATE_KEY=<your hex private key>
export NOSTR_USERNAME=alice          # defaults to first 8 chars of pubkey
export LOCAL_DOMAIN=https://your-domain.com
./klistr
```

Optional environment variables:
```bash
# Profile metadata
NOSTR_DISPLAY_NAME=<full name>
NOSTR_SUMMARY=<bio>
NOSTR_PICTURE=<profile picture URL>
NOSTR_BANNER=<banner image URL>

# Relay config (comma-separated for multiple relays)
NOSTR_RELAY=wss://relay1.example.com,wss://relay2.example.com

# Bluesky bridge (optional — both must be set to enable)
BSKY_IDENTIFIER=user.bsky.social    # Bluesky handle or DID
BSKY_APP_PASSWORD=xxxx-xxxx-xxxx-xxxx  # Bluesky app password (Settings → App Passwords)

# Web admin UI (optional — omit to disable /web entirely)
WEB_ADMIN=<password>            # Enables /web admin dashboard; HTTP Basic Auth password

# Other
LOG_LEVEL=info|debug            # slog structured output level
EXTERNAL_BASE_URL=https://njump.me  # Base URL for Nostr links
SIGN_FETCH=true                 # Sign outbound AP requests (default: true)
ZAP_PUBKEY=<hex>                # Optional Lightning zap split recipient
ZAP_SPLIT=0.1                   # Zap split percentage (default 10%)
```

## Architecture

klistr is a single-user personal bridge between Nostr and ActivityPub (Fediverse) and optionally Bluesky (AT Protocol). It runs as a single binary, acting as an **ActivityPub server** and a **Nostr client** simultaneously — bridging one Nostr identity to one AP actor.

### Data Flow

```
AP (Mastodon etc.)  →  POST /inbox  →  ap.APHandler  →  Nostr relay (publish event)
Nostr relay (author filter)  →  nostr.Handler  →  ap.Federator  →  AP inboxes (HTTP POST)
Nostr relay (author filter)  →  nostr.Handler  →  bsky.Poster  →  Bluesky XRPC POST
Bluesky notifications poll (30s)  →  bsky.Poller  →  nostr.Publisher  →  relays
```

### Package Overview

- **`cmd/klistr/`** — Entry point. Wires all components together: loads config, opens DB, initializes RSA keys, creates handlers, starts relay pool, optional Bluesky bridge, and HTTP server.
- **`internal/config/`** — Environment variable configuration. `config.Load()` exits on missing `NOSTR_PRIVATE_KEY`. Derives `NostrPublicKey` automatically. `BskyEnabled()` returns true when both `BSKY_IDENTIFIER` and `BSKY_APP_PASSWORD` are set.
- **`internal/db/`** — Database layer (`db.Store`). Supports SQLite (default, WAL mode) and PostgreSQL. Four tables: `objects` (AP/AT↔Nostr event ID), `follows`, `actor_keys` (derived Nostr pubkey → AP actor URL, for NIP-05 lookups during kind-3 processing), `kv` (key-value store for persistent state like Bluesky notification cursor). Uses `sync.Map` caches to reduce DB round-trips. SQL placeholders differ by driver (`?` vs `$1`, selected via `ph()` helper).
- **`internal/ap/`** — ActivityPub logic:
  - `transmute.go` — Converts Nostr events → AP objects (`ToActor`, `ToNote`, `ToAnnounce`, `ToLike`, etc.) and builds AP activities (`BuildCreate`, `BuildUpdate`, `BuildFollow`, `BuildAccept`). Uses `TransmuteContext` (holds `LocalDomain`, `LocalActorURL`, `PublicKeyPem`, and an object-ID-lookup callback).
  - `handler.go` — `APHandler`: receives incoming AP activities, converts them to Nostr events, publishes to relays. Handles Follow/Unfollow/Delete/Like/Announce. On Follow, notifies local user via NIP-04 DM to self.
  - `federation.go` — `Federator`: delivers AP activities outbound. Resolves follower lists, fetches actor inboxes, deduplicates by shared inbox per origin.
  - `client.go` — HTTP client for fetching AP actors/objects with in-memory caching. Defines `ErrGone` for HTTP 410 responses (deleted actors); signature verification skips and accepts activities from gone actors.
  - `crypto.go` / `keys.go` — RSA key management; auto-generates key pair if PEM files don't exist.
  - `types.go` — AP type definitions. `StringOrArray` provides flexible JSON deserialization for `To`/`CC` fields that may be a string or array depending on AP server.
- **`internal/bsky/`** — Bluesky (AT Protocol) bridge (optional):
  - `types.go` — Bluesky XRPC request/response structs (Session, FeedPost, Facet, LikeRecord, RepostRecord, Notification, etc.).
  - `client.go` — Thin XRPC HTTP client. `Authenticate` creates a session via `com.atproto.server.createSession`; re-authenticates automatically on 401. Methods: `CreateRecord`, `DeleteRecord`, `ListNotifications`, `GetProfile`.
  - `transmute.go` — Conversion between Nostr events and Bluesky records. `NostrNoteToFeedPost` truncates to 300 graphemes, builds URL/hashtag facets, resolves reply threading. `NotificationToNostrEvent` maps like/repost/reply/mention → Nostr kinds 7/6/1 with `["proxy", atURI, "atproto"]` tag.
  - `poster.go` — `Poster`: outbound bridge. Handles kind-1 (post), kind-5 (delete), kind-6 (repost), kind-7 "+" (like). Guards against double-bridging via `GetAPIDForObject`. Stores AT URI ↔ Nostr event ID mappings.
  - `poller.go` — `Poller`: inbound bridge. Polls `app.bsky.notification.listNotifications` every 30s. Converts notifications to Nostr events and publishes them. Sends NIP-04 self-DM on new follower. Saves cursor to `kv` table for resumption.
- **`internal/nostr/`** — Nostr protocol handling:
  - `signer.go` — `Signer`: dual signing — `SignAsUser` uses the real private key; `Sign(event, apID)` derives a deterministic key via `SHA-256(localPrivKey + ":" + apActorID)`. Derived keys cached with `sync.RWMutex`. Also provides `CreateDMToSelf()` (NIP-04 encrypted kind-4 event) for follower notifications.
  - `relay.go` — `RelayPool`: subscribes to a single author's events (kinds 0,1,3,5,6,7,9735) with author filter. Auto-reconnects with 5s backoff. `Publisher`: publishes events to write relays.
  - `handler.go` — `Handler`: processes Nostr events. Skips events with `proxy` tag (`IsProxyEvent()`). Optionally mirrors to Bluesky via `BskyPoster` interface.
- **`internal/server/`** — Chi-based HTTP server. Endpoints:
  - `/.well-known/webfinger`, `/.well-known/host-meta`, `/.well-known/nodeinfo`, `/.well-known/nostr.json` (NIP-05)
  - `GET/POST /users/{username}` — Actor profile and inbox
  - `GET /users/{username}/followers|following|outbox`
  - `GET /objects/{id}` — AP Note objects
  - `GET /api/healthcheck`
  - Returns 404 for any username that isn't the configured `NostrUsername`.

### Identity

- **Local AP actor URL**: `https://LOCAL_DOMAIN/users/<NostrUsername>`
- **Nostr event → AP URL**: `https://LOCAL_DOMAIN/objects/<event-id>`
- **AP actor → Nostr keypair**: deterministic derivation via `Signer` (seed = `NostrPrivateKey + ":" + apActorID`)

### Loop Prevention

Three layers:
1. **Relay level**: subscription filtered to local author's pubkey only
2. **Handler level**: `IsProxyEvent()` skips events with `["proxy", ..., "activitypub"]` tag; Bluesky inbound events carry `["proxy", atURI, "atproto"]` tag (also caught by `IsProxyEvent`)
3. **Bluesky outbound**: `GetAPIDForObject(event.ID)` guard — if an AT URI is already stored for a Nostr event ID, the post is skipped

### Database

- SQLite path detection: bare filename → SQLite, `postgres://` prefix → PostgreSQL
- SQLite is single-writer (`SetMaxOpenConns(1)`) with WAL mode
- Migrations are idempotent (`CREATE TABLE IF NOT EXISTS`, `INSERT OR IGNORE`)
- Follows are stored with full AP actor URLs as both `follower_id` and `followed_id`

### NIP-04 Self-Notification

When a new Fediverse Follow is received, `handleFollow()` asynchronously sends a NIP-04 encrypted kind-4 DM to the local user's own pubkey: `"🔔 New Fediverse follower: @username@domain"`. The shared secret is derived from the local pubkey/privkey pair (self-addressed).

## Module

```
module github.com/klppl/klistr
go 1.23.1
```

Key dependencies: `go-chi/chi` (HTTP router), `nbd-wtf/go-nostr` (Nostr protocol, includes NIP-04), `modernc.org/sqlite` (pure-Go SQLite, no CGO), `go-fed/httpsig` (HTTP Signatures for AP), `lib/pq` (PostgreSQL driver).
