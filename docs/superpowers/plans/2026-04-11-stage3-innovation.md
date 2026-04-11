# Stage 3: Innovation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add differentiating features that make klistr superior to mostr and other bridges — zap bridging, cross-protocol mute sync, interaction-driven profile refresh, and deep thread ancestry walking.

**Architecture:** Extends existing handlers with new event kinds (10000), new XRPC methods (muteActor/unmuteActor), enhanced cache staleness checks, and deeper reply chain resolution. All outbound deliveries flow through the Stage 1 outbox queue.

**Tech Stack:** Go 1.24, existing `go-nostr`, Bluesky XRPC `app.bsky.graph.muteActor`/`unmuteActor`.

---

## Task 1: Enhanced Zap Bridging (NIP-57)

**Files:**
- Modify: `internal/ap/transmute.go` — Enhance `ToZap` with dual-type array and Like fallback
- Modify: `internal/bsky/poster.go` — Add `handleKind9735` for Nostr→Bluesky zap→like
- Modify: `internal/nostr/handler.go` — Wire kind-9735 to BskyPoster

The existing `ToZap` produces a custom "Zap" activity type that most AP servers ignore. Enhance it with a fallback so non-Zap-aware servers see a Like.

### Changes to `ToZap` in transmute.go:

Change the activity type from a single string to an array for dual-type:
```go
// Before: "type": "Zap",
// After:
"type": []string{"Zap", "Like"},
```

Add structured amount data as extension properties:
```go
if amountMsats > 0 {
    act["amount"] = map[string]interface{}{
        "value":    amountMsats,
        "currency": "millisatoshi",
    }
}
if bolt11 != "" {
    act["bolt11"] = bolt11
}
```

Extract bolt11 from the kind-9735 tags:
```go
var bolt11 string
for _, tag := range event.Tags {
    if len(tag) >= 2 && tag[0] == "bolt11" {
        bolt11 = tag[1]
    }
}
```

### Add handleKind9735 to poster.go:

```go
func (p *Poster) handleKind9735(ctx context.Context, event *nostr.Event) {
    // Zap receipts become likes on Bluesky (no Lightning on AT Protocol)
    if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
        return
    }
    var zappedNostrID string
    for _, tag := range event.Tags {
        if len(tag) >= 2 && tag[0] == "e" {
            zappedNostrID = tag[1]
            break
        }
    }
    if zappedNostrID == "" {
        return
    }
    atURI, ok := p.Store.GetAPIDForObject(zappedNostrID)
    if !ok || !strings.HasPrefix(atURI, "at://") {
        return
    }
    rec := LikeRecord{
        Type:      likeType,
        Subject:   Ref{URI: atURI},
        CreatedAt: event.CreatedAt.Time().UTC().Format(time.RFC3339),
    }
    resp, err := p.Client.CreateRecord(ctx, CreateRecordRequest{
        Repo:       p.Client.DID(),
        Collection: "app.bsky.feed.like",
        Record:     rec,
    })
    if err != nil {
        slog.Warn("bsky: failed to bridge zap as like", "id", event.ID, "error", err)
        return
    }
    p.Store.AddObject(resp.URI, event.ID)
}
```

Add kind 9735 to the Handle/HandleDirect switch statements in poster.go:
```go
case 9735:
    p.handleKind9735(ctx, event)
```

Also add a `handleKind9735Direct` variant that returns error for outbox retry.

Commit: `git commit -m "bridge: enhance zap bridging with dual-type Like fallback and Bluesky like"`

---

## Task 2: Cross-Protocol Mute Sync (NIP-51, kind-10000)

**Files:**
- Modify: `internal/nostr/relay.go` — Add kind 10000 to subscription filter
- Modify: `internal/nostr/handler.go` — Add `handleKind10000`
- Modify: `internal/bsky/client.go` — Add `MuteActor`/`UnmuteActor` methods
- Modify: `internal/ap/transmute.go` — Add `ToBlock`/`BuildUndoBlock` functions

### Add kind 10000 to relay subscription:

In `internal/nostr/relay.go`, change the Kinds array:
```go
// Before: Kinds: []int{0, 1, 3, 5, 6, 7, 1068, 9735, 10002, 30023},
// After:  Kinds: []int{0, 1, 3, 5, 6, 7, 1068, 9735, 10000, 10002, 30023},
```

### Add MuteActor/UnmuteActor to bsky/client.go:

```go
func (c *Client) MuteActor(ctx context.Context, did string) error {
    body := map[string]string{"actor": did}
    return c.xrpcPost(ctx, "app.bsky.graph.muteActor", body)
}

func (c *Client) UnmuteActor(ctx context.Context, did string) error {
    body := map[string]string{"actor": did}
    return c.xrpcPost(ctx, "app.bsky.graph.unmuteActor", body)
}
```

Note: `xrpcPost` may not exist as a named method — adapt to the existing pattern (likely `authedPost` with JSON body). These XRPC endpoints return 200 with empty body on success.

### Add ToBlock/BuildUndoBlock to ap/transmute.go:

```go
func ToBlock(actorURL string, tc *TransmuteContext) map[string]interface{} {
    return map[string]interface{}{
        "@context": DefaultContext,
        "id":       tc.LocalActorURL + "#block-" + extractID(actorURL),
        "type":     "Block",
        "actor":    tc.LocalActorURL,
        "object":   actorURL,
    }
}

func BuildUndoBlock(actorURL string, tc *TransmuteContext) map[string]interface{} {
    blockActivity := ToBlock(actorURL, tc)
    return map[string]interface{}{
        "@context": DefaultContext,
        "id":       tc.LocalActorURL + "#undo-block-" + extractID(actorURL),
        "type":     "Undo",
        "actor":    tc.LocalActorURL,
        "object":   blockActivity,
    }
}
```

### Add handleKind10000 to nostr/handler.go:

```go
func (h *Handler) handleKind10000(ctx context.Context, event *nostr.Event) {
    // NIP-51 mute list: extract muted pubkeys, diff with stored state,
    // propagate adds/removes to Bluesky and AP.
    
    // 1. Extract current muted pubkeys from event p-tags
    currentMutes := make(map[string]struct{})
    for _, tag := range event.Tags {
        if len(tag) >= 2 && tag[0] == "p" {
            currentMutes[tag[1]] = struct{}{}
        }
    }
    
    // 2. Load previously stored mutes from KV
    // Use kv key "muted_pubkeys" storing comma-separated hex pubkeys
    var previousMutes map[string]struct{}
    if stored, ok := h.Store.(interface{ GetKV(string) (string, bool) }); ok {
        if val, ok := stored.GetKV("muted_pubkeys"); ok && val != "" {
            previousMutes = make(map[string]struct{})
            for _, pk := range strings.Split(val, ",") {
                if pk != "" {
                    previousMutes[pk] = struct{}{}
                }
            }
        }
    }
    if previousMutes == nil {
        previousMutes = make(map[string]struct{})
    }
    
    // 3. Diff: find newly muted and newly unmuted
    var newMutes, removedMutes []string
    for pk := range currentMutes {
        if _, was := previousMutes[pk]; !was {
            newMutes = append(newMutes, pk)
        }
    }
    for pk := range previousMutes {
        if _, is := currentMutes[pk]; !is {
            removedMutes = append(removedMutes, pk)
        }
    }
    
    // 4. Store updated mute list
    if kv, ok := h.Store.(interface{ SetKV(string, string) error }); ok {
        var pks []string
        for pk := range currentMutes {
            pks = append(pks, pk)
        }
        kv.SetKV("muted_pubkeys", strings.Join(pks, ","))
    }
    
    // 5. Propagate new mutes
    for _, pk := range newMutes {
        // Resolve pubkey to AP actor URL (if known)
        if actorURL, ok := h.resolveAPActor(pk); ok {
            block := ap.ToBlock(actorURL, h.TC)
            h.Federator.Federate(ctx, block)
        }
        // Resolve pubkey to Bluesky DID (if known) and mute
        // This requires access to the BskyClient — may need a MuteHandler interface
    }
    
    // 6. Propagate removed mutes
    for _, pk := range removedMutes {
        if actorURL, ok := h.resolveAPActor(pk); ok {
            undoBlock := ap.BuildUndoBlock(actorURL, h.TC)
            h.Federator.Federate(ctx, undoBlock)
        }
    }
    
    slog.Info("mute list synced", "added", len(newMutes), "removed", len(removedMutes), "total", len(currentMutes))
}
```

Note: The exact implementation depends on what interfaces the Handler's Store satisfies. The Store is a `FollowStore` interface — you may need to type-assert to access GetKV/SetKV, or add those methods to the FollowStore interface. Read the existing code to determine the cleanest approach.

Add the case to the Handle switch:
```go
case 10000:
    h.handleKind10000(ctx, event)
```

Commit: `git commit -m "bridge: add cross-protocol mute sync via NIP-51 kind-10000"`

---

## Task 3: Interaction-Driven Profile Refresh

**Files:**
- Modify: `internal/ap/client.go` — Add staleness check to FetchActor

The current `FetchActor` uses a 1-hour TTL cache. When processing an incoming AP activity, if the actor's profile is stale (older than cache TTL), we should re-fetch inline.

The cache already handles this via TTL expiration — after `objectCacheTTL` (default 1h), the next `FetchObject` call re-fetches. This means interaction-driven refresh is **already working** for the object cache.

However, the `AccountResyncer` publishes kind-0 profiles on a 24h cycle. We can make it smarter: when an actor is fetched inline (cache miss), also re-publish the kind-0 immediately.

### Add a callback to FetchActor for cache-miss notifications:

```go
// In client.go, add a package-level callback:
var OnActorCacheMiss func(actorURL string)

// In FetchObject, after a successful fetch (cache miss path):
if OnActorCacheMiss != nil && strings.Contains(rawURL, "/users/") {
    OnActorCacheMiss(rawURL)
}
```

### Wire in main.go:

```go
ap.OnActorCacheMiss = func(actorURL string) {
    // Re-publish kind-0 for this actor when profile is re-fetched
    actor, err := ap.FetchActor(context.Background(), actorURL)
    if err != nil {
        return
    }
    event := ap.ActorToKind0Event(actor, signer, cfg.LocalDomain)
    if event != nil {
        publisher.Publish(context.Background(), event)
    }
}
```

Note: This is optional complexity. The simpler approach is to just rely on the existing cache TTL (1h) which already triggers re-fetches on interaction. The resyncer handles the kind-0 republish on a 24h cycle. **If this seems too complex for the existing architecture, skip the callback and just document that interaction-driven refresh is handled by the cache TTL.**

Decision: Implement the simple version — just verify that the cache TTL already provides interaction-driven refresh and document it. No code changes needed.

Commit: Skip (no code changes) or document-only commit.

---

## Task 4: Deep Thread Ancestry Walking (10 Levels)

**Files:**
- Modify: `internal/ap/handler.go` — Extend noteToEvent to walk InReplyTo chains up to 10 levels

Currently `noteToEvent` walks 2 levels (parent → grandparent). Extend to walk up to 10 levels to reconstruct full thread ancestry, matching the Bluesky poller's `parentHeight=10`.

### Changes to noteToEvent:

Replace the current 2-level walk with a loop:

```go
// After resolving replyToEventID from note.InReplyTo:
// Walk the chain up to maxAncestorDepth levels to find the true root
const maxAncestorDepth = 10
rootEventID := replyToEventID
currentURL := note.InReplyTo

for depth := 0; depth < maxAncestorDepth; depth++ {
    parentObj, err := FetchObject(ctx, currentURL)
    if err != nil {
        break
    }
    parentNote := mapToNote(parentObj)
    if parentNote == nil || parentNote.InReplyTo == "" {
        break // reached the root
    }
    if parentID, ok := h.resolveNostrID(parentNote.InReplyTo); ok {
        rootEventID = parentID
    }
    currentURL = parentNote.InReplyTo
}
```

This replaces the existing single-level grandparent check. The `FetchObject` calls hit the cache (1h TTL), so previously-fetched ancestors are free. Only truly deep threads trigger multiple HTTP fetches.

Commit: `git commit -m "ap: extend thread ancestry walking to 10 levels for better thread reconstruction"`

---

## Task 5: Update CLAUDE.md and Tests

**Files:**
- Modify: `CLAUDE.md`
- Create: `internal/nostr/handler_test.go` (optional — mute sync test)

Document Stage 3 features. Update the relay subscription filter documentation to include kind 10000. Note the zap bridging enhancement and mute sync.

Commit: `git commit -m "docs: document Stage 3 innovation features in CLAUDE.md"`
