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
SHOW_SOURCE_LINK=true           # Append original post URL (🔗) at the bottom of bridged notes (default: false)
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
- **`internal/config/`** — Environment variable configuration. `config.Load()` exits on missing `NOSTR_PRIVATE_KEY`. Derives `NostrPublicKey` automatically. `BskyEnabled()` returns true when both `BSKY_IDENTIFIER` and `BSKY_APP_PASSWORD` are set. `ShowSourceLink bool` is set from `SHOW_SOURCE_LINK` env var and propagated to `APHandler` and `Poller`.
- **`internal/db/`** — Database layer (`db.Store`). Supports SQLite (default, WAL mode) and PostgreSQL. Four tables: `objects` (AP/AT↔Nostr event ID), `follows`, `actor_keys` (derived Nostr pubkey → AP actor URL, for NIP-05 lookups during kind-3 processing), `kv` (key-value store for persistent state like the Bluesky last-seen timestamp). Uses `sync.Map` caches to reduce DB round-trips. SQL placeholders differ by driver (`?` vs `$1`, selected via `ph()` helper). `Stats(followedID)` returns per-bridge counts: `FediverseObjects` (`ap_id LIKE 'http%'`), `BskyObjects` (`ap_id LIKE 'at://%'`), `TotalObjects`, `FollowerCount`, `ActorKeyCount`, and `BskyLastSeen` (from kv). `GetAPFollowing(followerID)` returns only AP follows (`followed_id LIKE 'http%'`); `GetBskyFollowing(followerID)` returns only Bluesky follows (`followed_id LIKE 'bsky:%'`). Bluesky follows are stored as `bsky:<did>` with associated kv keys `bsky_follow_<did>` (rkey for deletion) and `bsky_follow_handle_<did>` (display handle).
- **`internal/ap/`** — ActivityPub logic:
  - `transmute.go` — Converts Nostr events → AP objects (`ToActor`, `ToNote`, `ToAnnounce`, `ToLike`, etc.) and builds AP activities (`BuildCreate`, `BuildUpdate`, `BuildFollow`, `BuildAccept`). Uses `TransmuteContext` (holds `LocalDomain`, `LocalActorURL`, `PublicKeyPem`, and an object-ID-lookup callback).
  - `handler.go` — `APHandler`: receives incoming AP activities, converts them to Nostr events, publishes to relays. Handles Follow/Unfollow/Delete/Like/Announce. On Follow, notifies local user via NIP-04 DM to self. `noteToEvent` synchronously pre-fetches `InReplyTo`/`QuoteURL` parent objects before converting (preventing race-condition drops), extracts `<a href>` URLs hidden behind anchor text (skipping mention actor URLs and hashtag search paths), and optionally appends the original post URL when `ShowSourceLink` is set. `Note` struct has a `URL` field (human-readable post URL, may differ from AP `id` on some servers). `Announce` (repost) is also pre-fetched synchronously so the Nostr ID lookup succeeds.
  - `resync.go` — `AccountResyncer`: runs every 24h (and on manual trigger) — iterates all `actor_keys` AP URLs, re-fetches each actor via HTTP, re-publishes kind-0 with fresh profile data. Stores `last_resync_at` and `last_resync_count` in the `kv` table.
  - `federation.go` — `Federator`: delivers AP activities outbound. Resolves follower lists, fetches actor inboxes, deduplicates by shared inbox per origin.
  - `client.go` — HTTP client for fetching AP actors/objects with in-memory caching. Defines `ErrGone` for HTTP 410 responses (deleted actors); signature verification skips and accepts activities from gone actors.
  - `crypto.go` / `keys.go` — RSA key management; auto-generates key pair if PEM files don't exist.
  - `types.go` — AP type definitions. `StringOrArray` provides flexible JSON deserialization for `To`/`CC` fields that may be a string or array depending on AP server.
- **`internal/bsky/`** — Bluesky (AT Protocol) bridge (optional):
  - `types.go` — Bluesky XRPC request/response structs (Session, FeedPost, Facet, LikeRecord, RepostRecord, Notification, etc.).
  - `client.go` — Thin XRPC HTTP client. `Authenticate` creates a session via `com.atproto.server.createSession`; re-authenticates automatically on 401. Methods: `CreateRecord`, `DeleteRecord`, `ListNotifications`, `GetProfile`, `GetTimeline`.
  - `transmute.go` — Conversion between Nostr events and Bluesky records. `NostrNoteToFeedPost` truncates to 300 graphemes, builds URL/hashtag facets, resolves reply threading. `NotificationToNostrEvent` maps like/repost → Nostr kinds 7/6 with `["proxy", atURI, "atproto"]` tag. `extractReplyRefs` pulls parent/root AT URIs from a reply record for thread reconstruction. `extractContentFromRecord(record)` extracts post text and appends any link URIs that only appear in rich-text facets (`app.bsky.richtext.facet#link`) or external embed link cards (`app.bsky.embed.external`) but are absent from the plain text. `extractImagesFromRecord(record, authorDID)` returns `[]ImageInfo` for images in `app.bsky.embed.images` embeds; `blobToCDNURL` constructs `cdn.bsky.app` URLs from author DID + blob CID + mimeType.
  - `poster.go` — `Poster`: outbound bridge. Handles kind-1 (post), kind-5 (delete), kind-6 (repost), kind-7 "+" (like). Guards against double-bridging via `GetAPIDForObject`. Stores AT URI ↔ Nostr event ID mappings.
  - `poller.go` — `Poller`: inbound bridge. Two polling paths: (1) `pollNotifications` — `app.bsky.notification.listNotifications` every 30s; uses `bsky_last_seen_at` kv key to skip already-processed items. Like/repost → Nostr kind-7/6. Reply → threaded Nostr kind-1 signed with a derived key for the Bluesky author's DID (stores AT URI ↔ Nostr event ID); falls back to NIP-04 self-DM if parent not in DB. Mention/quote → NIP-04 self-DM. New follower → NIP-04 self-DM. (2) `pollTimeline` — `app.bsky.feed.getTimeline`; bridges posts from followed accounts as kind-1 events signed with derived keys; uses `bsky_timeline_last_seen_at` kv key. Both paths: call `extractContentFromRecord` (text + hidden link URLs), `extractImagesFromRecord` (image blobs → NIP-94 `imeta` tags + CDN URLs appended to content), and `buildImeta` helper. Optional `ShowSourceLink` field appends `🔗 bsky.app` URL to content. Optional `TriggerCh` triggers immediate poll (used by web admin UI).
- **`internal/nostr/`** — Nostr protocol handling:
  - `signer.go` — `Signer`: dual signing — `SignAsUser` uses the real private key; `Sign(event, apID)` derives a deterministic key via `SHA-256(localPrivKey + ":" + apActorID)`. Derived keys cached with `sync.RWMutex`. Also provides `CreateDMToSelf()` (NIP-04 encrypted kind-4 event) for follower notifications.
  - `relay.go` — `RelayPool`: subscribes to a single author's events (kinds 0,1,3,5,6,7,9735) with author filter. Auto-reconnects with 5s backoff. Supports dynamic relay list changes via `AddRelay`/`RemoveRelay`; sends on a `restartCh` channel to cancel and immediately re-subscribe with the new list (no 5s delay). `Publisher`: publishes events to write relays with per-relay circuit breaker (`relayCircuit`, threshold=3 failures → open for 5 min, half-open auto-retry). Skips open circuits silently after logging "circuit opened" once; logs "relay recovered" on success. `RelayStatus` type exported for admin UI. `AddRelay`/`RemoveRelay`/`Relays()`/`RelayStatuses()`/`ResetCircuit()` methods.
  - `handler.go` — `Handler`: processes Nostr events. Skips events with `proxy` tag (`IsProxyEvent()`). Optionally mirrors to Bluesky via `BskyPoster` interface. `FollowStore` interface uses `GetAPFollowing` (not `GetFollowing`) so Bluesky `bsky:<did>` entries in the `follows` table do not trigger spurious AP Undo Follow activities when kind-3 is processed.
- **`internal/server/`** — Chi-based HTTP server. Endpoints:
  - `/.well-known/webfinger`, `/.well-known/host-meta`, `/.well-known/nodeinfo`, `/.well-known/nostr.json` (NIP-05)
  - `GET/POST /users/{username}` — Actor profile and inbox
  - `GET /users/{username}/followers|following|outbox`
  - `GET /objects/{id}` — AP Note objects
  - `GET /api/healthcheck`
  - Returns 404 for any username that isn't the configured `NostrUsername`.
  - `admin.go` — Web admin UI (mounted at `/web` only when `WEB_ADMIN` is set). HTTP Basic Auth. Dashboard shows bridge-specific stat panels (Nostr/Fediverse/Bluesky/Total), uptime, followers with `@user@domain` formatting, quick links, log with level filters, a **Following** section with per-bridge follow/unfollow management, and a **Relays** section (per-relay status with circuit state badge, Test/Reset/Remove buttons, Add input — auto-refreshes every 15s). Endpoints: `GET /web/` (dashboard HTML), `GET /web/api/status` (includes `started_at` unix timestamp), `GET /web/api/stats` (per-bridge counts + `bsky_last_seen`), `GET /web/api/followers`, `POST /web/api/sync-bsky`, `GET /web/api/log` (ring-buffer snapshot as JSON array — fetched on demand, no SSE), `POST /web/api/import-following`, `GET /web/api/following`, `POST /web/api/follow`, `POST /web/api/unfollow`.
  - `relaymgr.go` — `RelayManager` interface + 5 relay management handlers: `GET /web/api/relays` (list with circuit status), `POST /web/api/relays` (add, validates `wss://` prefix), `DELETE /web/api/relays` (remove), `POST /web/api/relays/test` (connect test, returns latency), `POST /web/api/relays/reset-circuit` (reset open circuit). Relay list changes are persisted to `kv["nostr_relays"]` and reloaded at startup to override `NOSTR_RELAY` env var.
  - `followimport.go` — `FollowPublisher` interface + `handleImportFollowing` + shared `mergeAndPublishKind3` helper. `mergeAndPublishKind3(ctx, addPubkeys, removePubkeys)` fetches the existing kind-3 from relays, merges with AP and Bluesky DB follows (via `GetAPFollowing` + `GetBskyFollowing`), applies the add/remove lists, then signs and publishes; returns `(totalFollows, fetchedExisting, error)`. `handleImportFollowing` resolves a batch of Fediverse handles via WebFinger, stores `actor_keys` mappings, and delegates to this helper. `followPublisherAdapter` in `cmd/klistr/main.go` wires `Signer` + `Publisher` to the interface.
  - `followmanage.go` — Three handlers for individual follow management. `BskyClient` interface (satisfied by `*bsky.Client`) provides `FollowActor`, `DeleteRecord`, `GetProfile`, `DID`. **Fediverse follow**: WebFinger → actor URL → derive pubkey → `StoreActorKey` → `mergeAndPublishKind3`; `handleKind3` picks up the event and sends AP Follow automatically. **Fediverse unfollow**: accepts actor URL or `user@domain` → derive pubkey → `mergeAndPublishKind3`; `handleKind3` sends AP Undo Follow. **Bluesky follow**: `GetProfile` → DID → `FollowActor` → store rkey/handle in kv → `StoreActorKey` → `AddFollow(localActorURL, "bsky:"+did)` → `mergeAndPublishKind3`. **Bluesky unfollow**: resolve DID → look up rkey from kv → `DeleteRecord` → `RemoveFollow` → `mergeAndPublishKind3`. `Server.bskyClient` field (set via `SetBskyClient`) holds the interface; nil when Bluesky is not configured.
  - `logbroadcast.go` — `LogBroadcaster`: `io.Writer` that captures every slog line into a 500-line ring buffer. `Lines()` returns a snapshot for the `/web/api/log` endpoint. Wraps `os.Stdout` when `WEB_ADMIN` is set.

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

### NIP-04 Self-Notifications

Several events trigger a NIP-04 encrypted kind-4 DM to the local user's own pubkey (shared secret derived from the local pubkey/privkey pair — self-addressed):
- New Fediverse follower: `"🔔 New Fediverse follower: @username@domain"`
- New Bluesky follower: `"🔔 New Bluesky follower: @handle.bsky.social"`
- Bluesky mention or quote: `"💬 New Bluesky mention/quote from @handle: ..."`
- Bluesky reply where the parent post is not in the DB (fallback from thread bridging)

## Module

```
module github.com/klppl/klistr
go 1.23.1
```

Key dependencies: `go-chi/chi` (HTTP router), `nbd-wtf/go-nostr` (Nostr protocol, includes NIP-04), `modernc.org/sqlite` (pure-Go SQLite, no CGO), `go-fed/httpsig` (HTTP Signatures for AP), `lib/pq` (PostgreSQL driver).
