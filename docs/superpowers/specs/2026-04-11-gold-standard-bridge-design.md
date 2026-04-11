# klistr Gold Standard Bridge — Design Spec

**Date:** 2026-04-11
**Goal:** Transform klistr from a functional personal bridge into the most robust, feature-complete Nostr↔ActivityPub↔Bluesky bridge in the ecosystem.
**Approach:** Reliability → Protocol Fidelity → Innovation (A→B→C)

---

## Stage 1: Reliability — Unified Outbox Queue

### Problem

All three outbound delivery paths (AP federation, relay publish, Bluesky XRPC) are fire-and-forget. A single network glitch means a permanently lost activity. No persistence across restarts. No retry. No observability into failed deliveries.

### Solution: `internal/outbox/` Package

A DB-backed outbox table with a priority worker pool. Every outbound delivery flows through the outbox — enqueue returns immediately, workers drain asynchronously.

### Schema

```sql
-- SQLite version (PostgreSQL uses SERIAL instead of AUTOINCREMENT;
-- existing ph() helper handles placeholder differences)
CREATE TABLE IF NOT EXISTS outbox (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    dest_type       TEXT NOT NULL,           -- 'ap', 'relay', 'bsky'
    dest_url        TEXT NOT NULL,           -- target inbox/relay/PDS URL
    payload         TEXT NOT NULL,           -- JSON (see Payload Format below)
    priority        INTEGER NOT NULL DEFAULT 1,
    status          TEXT NOT NULL DEFAULT 'pending',
    attempts        INTEGER NOT NULL DEFAULT 0,
    max_attempts    INTEGER NOT NULL DEFAULT 6,
    next_retry_at   TEXT NOT NULL,           -- RFC3339
    last_error      TEXT,
    created_at      TEXT NOT NULL,
    completed_at    TEXT,
    source_event_id TEXT                     -- correlation ID (Nostr event ID or AP activity ID)
);
CREATE INDEX IF NOT EXISTS outbox_drain
    ON outbox(status, next_retry_at, priority);
```

### Payload Format

The `payload` column stores JSON whose structure depends on `dest_type`:

- **`ap`:** Serialized JSON-LD activity (the full AP object as sent to inboxes). Worker deserializes and POSTs with HTTP Signature.
- **`relay`:** Serialized Nostr event JSON (the signed event). Worker deserializes to `nostr.Event` and calls relay publish.
- **`bsky`:** JSON object with `method` (XRPC method path, e.g. `com.atproto.repo.createRecord`) and `body` (request payload). Worker calls the appropriate client method.

### Priority Lanes

| Priority | Label | Examples |
|----------|-------|---------|
| 0 | real-time | Replies, likes, reposts, follows, unfollows, deletes |
| 1 | normal | New posts, profile updates |
| 2 | background | Resyncs, timeline bridges, bulk imports |

### Worker Pool

- **AP workers:** N goroutines (default 10, from `AP_FEDERATION_CONCURRENCY`)
- **Relay workers:** M goroutines (default 5)
- **Bluesky workers:** 1 goroutine (single PDS, rate-limited)

Each worker:
1. `SELECT ... WHERE status='pending' AND dest_type=? AND next_retry_at <= now() ORDER BY priority, next_retry_at LIMIT 1`
2. `UPDATE status='claimed'` (SQLite single-writer makes this atomic)
3. Attempt delivery
4. Success → `status='done', completed_at=now()`
5. Failure → `attempts++`, compute `next_retry_at` from backoff schedule, `status='pending'`
6. If `attempts >= max_attempts` → `status='dead'`

Workers poll with 100ms sleep when idle.

### Backoff Schedule

| Attempt | Delay | Cumulative |
|---------|-------|-----------|
| 1 | immediate | 0s |
| 2 | 5s | 5s |
| 3 | 30s | 35s |
| 4 | 2m | 2m 35s |
| 5 | 10m | 12m 35s |
| 6 | 1h | 1h 12m 35s |
| 7+ | dead letter | — |

### Fan-Out

Fan-out happens at enqueue time:
- **AP:** Federator resolves follower inboxes, deduplicates shared inboxes, enqueues one row per inbox
- **Relay:** Publisher enqueues one row per write relay
- **Bluesky:** Poster enqueues one row to the PDS URL

### Circuit Breaker Integration

The existing relay circuit breaker becomes a fast-fail gate:
- Worker claims row for relay X → checks circuit state
- If **open**: reschedule with short backoff (no network attempt wasted)
- If **closed/half-open**: attempt delivery, update circuit state from result

### Cleanup

Goroutine every 6 hours:
- Purge `status='done'` rows older than 48 hours
- Purge `status='dead'` rows older than 7 days

### Admin UI Additions

- **Outbox panel:** pending/in-flight/dead counts per dest_type
- **Dead Letter view:** list dead items with last error, manual retry button
- **Delivery latency:** p50/p95 from `completed_at - created_at`

### Integration Changes

| Component | Before | After |
|-----------|--------|-------|
| `federation.go` `Federate()` | HTTP POST per inbox | `outbox.Enqueue()` per inbox |
| `relay.go` `Publish()` | Direct relay write with circuit breaker | `outbox.Enqueue()` per relay |
| `poster.go` `Handle()` | Direct XRPC POST | `outbox.Enqueue()` to PDS |
| Circuit breaker | Retry mechanism | Fast-fail gate only |

### Files

- `internal/outbox/outbox.go` — `Queue` struct: `Enqueue()`, `Claim()`, `Complete()`, `Fail()`, `DeadLetter()`, `Stats()`
- `internal/outbox/worker.go` — `WorkerPool` struct: per-dest-type goroutine pools, drain loop, shutdown
- `internal/outbox/cleanup.go` — periodic purge goroutine

---

## Stage 2: Protocol Fidelity

### 2.1 Outbound Images to Bluesky

**Problem:** `poster.go` has zero image handling. Kind-1 events with image attachments post as plain text.

**Solution:** Parse imeta tags from Nostr events, download images (10s timeout, 20MB cap), upload as blobs via `com.atproto.repo.uploadBlob`, attach as `app.bsky.embed.images` with alt-text and aspect ratio. Max 4 images (Bluesky limit).

**Files changed:**
- `internal/bsky/client.go` — new `UploadBlob(ctx, reader, mimeType) (*BlobRef, error)` method
- `internal/bsky/poster.go` — `handleKind1` parses imeta tags, downloads, uploads, builds embed
- `internal/bsky/types.go` — `BlobRef`, `ImageEmbed` structs

### 2.2 Quote Posts to Bluesky

**Problem:** Nostr events with q-tags are treated as plain posts, not proper quote posts.

**Solution:** When a kind-1 event has a q-tag, resolve the quoted event ID to an AT URI via the objects table. Build `app.bsky.embed.record` (or `app.bsky.embed.recordWithMedia` if images are also present).

**Files changed:**
- `internal/bsky/poster.go` — `handleKind1` checks for q-tag, builds record embed
- `internal/bsky/transmute.go` — new `buildQuoteEmbed()` helper

### 2.3 Thread Resilience — Retry with Fallback

**Problem:** Replies with unresolvable parents are silently dropped (both AP→Nostr and Nostr→Bluesky directions).

**Solution:** Leverage the outbox queue with a custom retry profile for thread resolution. When parent resolution fails:
1. Enqueue the reply with `max_attempts=3` and a compressed backoff schedule (immediate, 5s, 30s) — shorter than the default 6-attempt schedule because parent propagation delay is typically under 10 seconds
2. On each retry, re-attempt parent resolution (parent may have arrived via relay propagation since last attempt)
3. After 3 failures, publish as top-level post with `"↩ [original URL]"` context link — strictly better than the current behavior of silently dropping the reply

This requires the outbox to support per-row `max_attempts` and a backoff strategy selector (default vs. thread-resolution).

**Files changed:**
- `internal/ap/handler.go` — `noteToEvent` returns a "needs retry" signal instead of dropping
- `internal/bsky/poster.go` — `handleKind1` reply path uses outbox retry
- `internal/outbox/outbox.go` — support for custom `max_attempts` per row

### 2.4 Content Warnings (NIP-36)

**Problem:** Nostr content-warning tags are ignored in both directions.

**Solution:**
- **Nostr→AP:** Map `["content-warning", "text"]` tag to AP Note `summary` field (Mastodon renders this as a CW natively)
- **Nostr→Bluesky:** Map to `com.atproto.label.defs#selfLabels` on the post record
- **AP→Nostr:** Map AP Note `summary` (when present alongside `content`) to `["content-warning", "summary text"]` tag
- **Bluesky→Nostr:** Map self-labels to content-warning tag

**Files changed:**
- `internal/ap/transmute.go` — `ToNote()` reads content-warning tag, sets `Summary`
- `internal/ap/handler.go` — `noteToEvent()` reads `Summary`, adds content-warning tag
- `internal/bsky/transmute.go` — `NostrNoteToFeedPost()` reads content-warning tag, builds self-labels
- `internal/bsky/poller.go` — `extractContentFromRecord()` reads self-labels, returns CW text

### 2.5 Mention Facets (Both Directions)

**Outbound (Nostr→Bluesky):** Best-effort. For each p-tag in the event, look up the pubkey in actor_keys. If the source is a Bluesky DID, generate a `facet#mention` with that DID. Only works for accounts we've already seen — honest and covers the common reply case.

**Inbound (Bluesky→Nostr):** When `extractContentFromRecord` encounters `facet#mention` with a DID, look up the derived pubkey for that DID in actor_keys. If found, add a p-tag to the Nostr event. Closes the mention round-trip.

**Files changed:**
- `internal/bsky/transmute.go` — `buildFacets()` accepts pubkey→DID resolver, generates mention facets
- `internal/bsky/poller.go` — mention facet extraction in `extractContentFromRecord`

### 2.6 NIP-05 for Bridged Bluesky Authors

**Problem:** `.well-known/nostr.json` only serves the local user. Bridged Bluesky authors with derived pubkeys can't be NIP-05 verified.

**Solution:** Extend the NIP-05 handler to query actor_keys for the requested name. If `name@localDomain` matches a Bluesky-derived actor, return its derived pubkey. Format: `{blueskyHandle}@{localDomain}`.

**Files changed:**
- `internal/server/server.go` — `.well-known/nostr.json` handler queries actor_keys for non-local-user names

### 2.7 Transmute Test Suite

Table-driven tests for protocol correctness. No tests currently exist — this is the highest-value addition.

**Coverage:**
- `internal/ap/transmute_test.go`:
  - `ToNote`: content, tags (mentions, hashtags, imeta), reply threading, content warnings, empty content
  - `ToActor`: all metadata fields, missing fields, NIP-05
  - `ToAnnounce`, `ToLike`, `ToEmojiReact`, `ToZap`, `ToQuestion`, `ToArticle`
  - `BuildCreate`, `BuildFollow`, `BuildAccept`, `BuildUndoFollow`
  - Edge cases: unicode content, max-length, malformed tags
- `internal/bsky/transmute_test.go`:
  - `NostrNoteToFeedPost`: grapheme truncation, facet byte offsets, reply building, hashtag extraction
  - `extractContentFromRecord`: hidden facet links, external embeds, plain text
  - `extractImagesFromRecord`: image blobs, video blobs, missing fields
  - `buildFacets`: URL regex, hashtag regex, mention facets
  - Edge cases: empty text, 300-grapheme boundary, multi-byte unicode
- `internal/outbox/outbox_test.go`:
  - `Enqueue`, `Claim`, `Complete`, `Fail`: state transitions, backoff calculation
  - Priority ordering, concurrent claim safety, cleanup

**Pattern:** Each test file uses a `[]struct{ name, input, expected }` table. Subtests via `t.Run(tc.name, ...)`.

---

## Stage 3: Innovation

### 3.1 NIP-57 Zap Bridging with Like Fallback

**Current state:** `ToZap()` exists in transmute.go, producing a custom `Zap` activity type.

**Enhancement:**
- **Outbound (Nostr→AP):** Emit `Zap` activity (mostr.pub namespace) with `type: ["Zap", "Like"]` so non-Zap-aware servers see a Like. Include `amount`, `bolt11`, and sender pubkey in extension properties.
- **Outbound (Nostr→Bluesky):** Zap receipts → `app.bsky.feed.like` (no Lightning on AT Protocol).
- **Inbound (AP→Nostr):** When an AP `Like` arrives with a `zap` extension property, convert to kind-9735 with amount data preserved. Plain `Like` stays kind-7.

**Files changed:**
- `internal/ap/transmute.go` — `ToZap()` enhanced with dual-type array and extension properties
- `internal/bsky/poster.go` — `handleKind9735()` bridges to Bluesky like
- `internal/ap/handler.go` — `handleLike()` checks for zap extension, routes to kind-9735 or kind-7

### 3.2 Cross-Protocol Mute Sync (NIP-51, kind-10000)

**Design:** Nostr as source of truth. User manages their mute list on Nostr; bridge propagates.

**Flow:**
1. Add kind-10000 to relay subscription filter
2. New `handleKind10000` in handler.go
3. Diff incoming mute list against stored state (new `mutes` kv entries)
4. New mutes → Bluesky `app.bsky.graph.muteActor` + enqueue AP `Block` activity
5. Removed mutes → Bluesky `app.bsky.graph.unmuteActor` + enqueue AP `Undo(Block)`

**Files changed:**
- `internal/nostr/relay.go` — add kind 10000 to subscription filter
- `internal/nostr/handler.go` — new `handleKind10000` case
- `internal/bsky/client.go` — new `MuteActor()`, `UnmuteActor()` methods
- `internal/ap/transmute.go` — new `ToBlock()`, `BuildUndoBlock()` functions

### 3.3 Enhanced Proxy Tags — "View Original" References

**Design:** Every bridged post carries machine-readable origin links for all three protocols.

**Tags added to Nostr events (inbound):**
- `["proxy", "<original URL>", "<protocol>"]` — existing, for loop prevention
- `["r", "<human-readable URL>"]` — reference tag for "View Original" in clients

**AP objects (outbound):**
- `proxyOf` array — existing
- `url` field — human-readable link (njump.me or client-specific)

**Specifics:**
- AP→Nostr: r-tag = the AP Note's `url` field (human page, not JSON-LD id)
- Bsky→Nostr: r-tag = `https://bsky.app/profile/{handle}/post/{rkey}`
- Nostr→AP: `url` = `https://njump.me/{nevent1...}` (configurable base)
- Nostr→Bsky: no action needed (Bluesky doesn't have a reference tag concept)

**Files changed:**
- `internal/ap/handler.go` — `noteToEvent` adds r-tag from Note.URL
- `internal/bsky/poller.go` — `bridgePost`/`bridgeReply` add r-tag with bsky.app URL
- `internal/ap/transmute.go` — `ToNote` sets `url` to njump/configurable link

### 3.4 Mostr Benchmark — Three Superior Implementations

**1. Announce/Repost Integrity:**
Mostr drops reposts when the original post isn't in its DB. klistr already pre-fetches synchronously. With the outbox queue, failed pre-fetches trigger a retry. After exhaustion, the repost publishes with the original AP URL as a link fallback instead of being dropped.

**2. Interaction-Driven Profile Refresh:**
Mostr resyncs on fixed intervals only. klistr adds: when an incoming AP activity references an actor whose cached profile is older than `AP_CACHE_TTL`, trigger an inline refresh before processing. Active conversationalists always have fresh profiles. Dormant accounts use the 24h cycle.

Implementation: `ap.Client.FetchActor()` checks `last_fetched_at` in cache metadata. If stale, re-fetch inline. Store `last_fetched_at` alongside cached actor.

**3. Deep Thread Ancestry Walking:**
Mostr walks `inReplyTo` chains only 1 level for root resolution. klistr extends to 10 levels (matching the Bluesky poller's `parentHeight=10`). For AP→Nostr, `noteToEvent` recursively resolves `inReplyTo` up to 10 ancestors, publishing intermediate kind-0 profiles and kind-1 notes as needed to reconstruct the full thread.

---

## New/Changed Files Summary

### New Files
| File | Purpose |
|------|---------|
| `internal/outbox/outbox.go` | Queue struct, Enqueue/Claim/Complete/Fail/Stats |
| `internal/outbox/worker.go` | WorkerPool, per-dest-type drain goroutines |
| `internal/outbox/cleanup.go` | Periodic purge goroutine |
| `internal/ap/transmute_test.go` | Table-driven tests for AP transmutation |
| `internal/bsky/transmute_test.go` | Table-driven tests for Bluesky transmutation |
| `internal/outbox/outbox_test.go` | Tests for queue operations and backoff |

### Changed Files
| File | Changes |
|------|---------|
| `internal/db/db.go` | Add outbox table migration |
| `internal/ap/federation.go` | Enqueue to outbox instead of direct HTTP |
| `internal/ap/handler.go` | Thread retry signal, zap extension detection, r-tag, deep ancestry walking, content warning mapping |
| `internal/ap/transmute.go` | Content warning in ToNote, enhanced ToZap, ToBlock/BuildUndoBlock, url field in ToNote |
| `internal/ap/resync.go` | Enqueue to outbox for resynced profiles |
| `internal/nostr/relay.go` | Add kinds 10000 to subscription filter, relay publish via outbox |
| `internal/nostr/handler.go` | handleKind10000 for mute sync, handleKind9735 routing to Bluesky |
| `internal/bsky/client.go` | UploadBlob, MuteActor, UnmuteActor methods |
| `internal/bsky/poster.go` | Image upload, quote embeds, content warnings, mention facets, outbox integration |
| `internal/bsky/poller.go` | Mention facet extraction, r-tag for origin links, content warning extraction |
| `internal/bsky/transmute.go` | buildQuoteEmbed, mention facets in buildFacets, self-labels for CW |
| `internal/bsky/types.go` | BlobRef, ImageEmbed, SelfLabel structs |
| `internal/server/server.go` | NIP-05 for bridged authors, outbox admin panel |
| `internal/server/admin.go` | Outbox stats panel, dead letter view |
| `cmd/klistr/main.go` | Wire outbox queue and worker pool, start cleanup goroutine |

---

## Environment Variables (New)

| Variable | Default | Purpose |
|----------|---------|---------|
| `OUTBOX_AP_WORKERS` | 10 | AP delivery worker count |
| `OUTBOX_RELAY_WORKERS` | 5 | Relay delivery worker count |
| `OUTBOX_BSKY_WORKERS` | 1 | Bluesky delivery worker count |
| `OUTBOX_CLEANUP_INTERVAL` | 6h | How often done/dead rows are purged |
| `OUTBOX_DONE_TTL` | 48h | How long completed rows are kept |
| `OUTBOX_DEAD_TTL` | 168h | How long dead-letter rows are kept |

---

## Non-Goals (Explicit Exclusions)

- **Multi-user support:** klistr remains single-user. The outbox serves one identity.
- **Distributed deployment:** No need for row-level locking beyond SQLite's single-writer. PostgreSQL support maintained but not optimized for concurrent workers.
- **Full NIP-96 media server:** We download and re-upload images, not host them. klistr is a bridge, not a media server.
- **Bluesky custom lexicons:** We don't invent AT Protocol record types. Zaps become likes on Bluesky.
- **AP extensions beyond mostr.pub namespace:** We use established extension patterns, not novel ones.
