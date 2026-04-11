# Stage 2: Protocol Fidelity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the biggest protocol gaps in klistr's transmute layer so bridged content renders natively on all three platforms — images, quote posts, content warnings, and mentions.

**Architecture:** Extends existing transmute/poster/poller code with new embed types, blob upload, and bidirectional mention resolution. Leverages the Stage 1 outbox queue for thread retry-with-fallback.

**Tech Stack:** Go 1.24, existing `go-nostr`, `modernc.org/sqlite`, Bluesky XRPC `com.atproto.repo.uploadBlob`.

---

## File Structure

### New Files
| File | Responsibility |
|------|---------------|
| `internal/ap/transmute_test.go` | Table-driven tests for AP transmutation functions |
| `internal/bsky/transmute_test.go` | Table-driven tests for Bluesky transmutation functions |

### Modified Files
| File | Changes |
|------|---------|
| `internal/bsky/types.go` | Add BlobRef, Embed, ImageEmbed, RecordEmbed, SelfLabel structs |
| `internal/bsky/client.go` | Add UploadBlob method |
| `internal/bsky/poster.go` | Image upload in postNote, quote post embed via q-tag, content warning via self-labels |
| `internal/bsky/transmute.go` | Add mention facets to buildFacets, add buildImageEmbed, buildQuoteEmbed, buildSelfLabels helpers |
| `internal/bsky/poller.go` | Extract mention facets → p-tags, extract self-labels → content-warning tag, add r-tag for "View Original" |
| `internal/ap/handler.go` | Thread retry-with-fallback (return retriable error instead of nil), add r-tag from Note.URL |
| `internal/server/server.go` | Extend NIP-05 handler for bridged Bluesky authors |

---

## Task 1: Bluesky Embed Types

**Files:**
- Modify: `internal/bsky/types.go`

- [ ] **Step 1: Add embed and blob types**

Add these types to `internal/bsky/types.go`:

```go
// BlobRef represents a Bluesky blob reference returned by uploadBlob.
type BlobRef struct {
	Type string `json:"$type"` // "blob"
	Ref  struct {
		Link string `json:"$link"`
	} `json:"ref"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// Embed is the polymorphic embed field on FeedPost.
type Embed struct {
	Type   string      `json:"$type"`
	Images []EmbedImage `json:"images,omitempty"`  // for app.bsky.embed.images
	Record *EmbedRef   `json:"record,omitempty"`   // for app.bsky.embed.record
	Media  *Embed      `json:"media,omitempty"`    // for app.bsky.embed.recordWithMedia
}

// EmbedImage is a single image within an embed.images.
type EmbedImage struct {
	Alt         string       `json:"alt"`
	Image       BlobRef      `json:"image"`
	AspectRatio *AspectRatio `json:"aspectRatio,omitempty"`
}

// AspectRatio for image/video embeds.
type AspectRatio struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// EmbedRef references another record (for quote posts).
type EmbedRef struct {
	URI string `json:"uri"`
	CID string `json:"cid,omitempty"`
}

// SelfLabels for content warnings on Bluesky posts.
type SelfLabels struct {
	Type   string      `json:"$type"` // "com.atproto.label.defs#selfLabels"
	Values []SelfLabel `json:"values"`
}

// SelfLabel is a single self-applied label.
type SelfLabel struct {
	Val string `json:"val"` // e.g. "sexual", "nudity", "graphic-media"
}
```

- [ ] **Step 2: Add Embed and Labels fields to FeedPost**

Modify the existing `FeedPost` struct to include:

```go
type FeedPost struct {
	Type      string      `json:"$type"`
	Text      string      `json:"text"`
	CreatedAt string      `json:"createdAt"`
	Facets    []Facet     `json:"facets,omitempty"`
	Reply     *Reply      `json:"reply,omitempty"`
	Langs     []string    `json:"langs,omitempty"`
	Embed     *Embed      `json:"embed,omitempty"`
	Labels    *SelfLabels `json:"labels,omitempty"`
}
```

- [ ] **Step 3: Build and verify**

Run: `go build ./...`
Expected: Compiles.

- [ ] **Step 4: Commit**

```bash
git add internal/bsky/types.go
git commit -m "bsky: add Embed, BlobRef, SelfLabels types for images, quotes, and content warnings"
```

---

## Task 2: Blob Upload

**Files:**
- Modify: `internal/bsky/client.go`

- [ ] **Step 1: Add UploadBlob method**

Add to `internal/bsky/client.go`:

```go
// UploadBlob uploads binary data to the PDS and returns a blob reference.
// Used for image embeds on posts. Max 1MB enforced by most PDS implementations.
func (c *Client) UploadBlob(ctx context.Context, data []byte, mimeType string) (*BlobRef, error) {
	url := c.PDSURL + "/xrpc/com.atproto.repo.uploadBlob"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mimeType)

	resp, err := c.authedRequest(req)
	if err != nil {
		return nil, fmt.Errorf("uploadBlob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("uploadBlob: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Blob BlobRef `json:"blob"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("uploadBlob: decode: %w", err)
	}
	return &result.Blob, nil
}
```

Note: `authedRequest` is the existing helper that adds the Bearer token and handles 401 re-auth. If it doesn't exist as a standalone function, you'll need to use the existing `authedPost`/`authedGet` pattern — check the existing code and adapt. The key is: POST with binary body, Authorization header, Content-Type set to the mime type.

- [ ] **Step 2: Build and verify**

Run: `go build ./...`

- [ ] **Step 3: Commit**

```bash
git add internal/bsky/client.go
git commit -m "bsky: add UploadBlob method for image embed support"
```

---

## Task 3: Outbound Images (Nostr → Bluesky)

**Files:**
- Modify: `internal/bsky/poster.go`
- Modify: `internal/bsky/transmute.go`

- [ ] **Step 1: Add image extraction helper to transmute.go**

Add a function that extracts imeta tags from a Nostr event and returns structured image info:

```go
// ExtractImetaTags parses NIP-94 imeta tags from a Nostr event.
// Returns up to maxImages entries with URL, alt text, mime type, and dimensions.
func ExtractImetaTags(event *nostr.Event, maxImages int) []imetaInfo {
	var result []imetaInfo
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "imeta" {
			continue
		}
		info := parseImetaFields(tag[1:])
		if info.URL != "" {
			result = append(result, info)
			if len(result) >= maxImages {
				break
			}
		}
	}
	return result
}

type imetaInfo struct {
	URL      string
	Alt      string
	MimeType string
	Width    int
	Height   int
}

func parseImetaFields(fields []string) imetaInfo {
	var info imetaInfo
	for _, field := range fields {
		parts := strings.SplitN(field, " ", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "url":
			info.URL = parts[1]
		case "alt":
			info.Alt = parts[1]
		case "m":
			info.MimeType = parts[1]
		case "dim":
			if wh := strings.SplitN(parts[1], "x", 2); len(wh) == 2 {
				info.Width, _ = strconv.Atoi(wh[0])
				info.Height, _ = strconv.Atoi(wh[1])
			}
		}
	}
	return info
}
```

- [ ] **Step 2: Add image download and upload to poster.go**

Add a method that downloads images and uploads them as blobs:

```go
// uploadImages downloads images from URLs and uploads them as Bluesky blobs.
// Returns an Embed suitable for FeedPost, or nil if no images.
func (p *Poster) uploadImages(ctx context.Context, images []imetaInfo) *Embed {
	if len(images) == 0 {
		return nil
	}

	var embedImages []EmbedImage
	httpClient := &http.Client{Timeout: 10 * time.Second}

	for _, img := range images {
		resp, err := httpClient.Get(img.URL)
		if err != nil {
			slog.Debug("bsky: failed to download image", "url", img.URL, "error", err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1_000_000)) // 1MB limit
		resp.Body.Close()
		if err != nil {
			continue
		}

		mimeType := img.MimeType
		if mimeType == "" {
			mimeType = resp.Header.Get("Content-Type")
		}
		if mimeType == "" {
			mimeType = "image/jpeg"
		}

		blob, err := p.Client.UploadBlob(ctx, data, mimeType)
		if err != nil {
			slog.Warn("bsky: failed to upload blob", "url", img.URL, "error", err)
			continue
		}

		ei := EmbedImage{
			Alt:   img.Alt,
			Image: *blob,
		}
		if img.Width > 0 && img.Height > 0 {
			ei.AspectRatio = &AspectRatio{Width: img.Width, Height: img.Height}
		}
		embedImages = append(embedImages, ei)
	}

	if len(embedImages) == 0 {
		return nil
	}

	return &Embed{
		Type:   "app.bsky.embed.images",
		Images: embedImages,
	}
}
```

- [ ] **Step 3: Wire image upload into postNote**

In `postNote`, after building the FeedPost via `NostrNoteToFeedPost`, add image handling:

```go
// After: post, err := NostrNoteToFeedPost(event, p.ExternalBaseURL, getATURI)
// Add:
images := ExtractImetaTags(event, 4)
if embed := p.uploadImages(ctx, images); embed != nil {
	post.Embed = embed
}
```

- [ ] **Step 4: Add required imports**

Add `"io"`, `"net/http"`, `"strconv"` to transmute.go imports if not present. Add `"io"`, `"net/http"` to poster.go imports.

- [ ] **Step 5: Build and verify**

Run: `go build ./...`

- [ ] **Step 6: Commit**

```bash
git add internal/bsky/poster.go internal/bsky/transmute.go
git commit -m "bsky: add outbound image support via imeta tag extraction and blob upload"
```

---

## Task 4: Quote Post Embeds (Nostr → Bluesky)

**Files:**
- Modify: `internal/bsky/poster.go`

- [ ] **Step 1: Add quote post detection and embed**

In `postNote`, after image handling, check for q-tag and build embed.record:

```go
// Check for quote post (q-tag)
var quoteATURI string
for _, tag := range event.Tags {
	if len(tag) >= 2 && tag[0] == "q" {
		if uri, ok := getATURI(tag[1]); ok && strings.HasPrefix(uri, "at://") {
			quoteATURI = uri
		}
		break
	}
}

if quoteATURI != "" {
	quoteEmbed := &Embed{
		Type:   "app.bsky.embed.record",
		Record: &EmbedRef{URI: quoteATURI},
	}
	if post.Embed != nil && post.Embed.Type == "app.bsky.embed.images" {
		// Both images and quote: use recordWithMedia
		post.Embed = &Embed{
			Type:   "app.bsky.embed.recordWithMedia",
			Record: &EmbedRef{URI: quoteATURI},
			Media:  post.Embed, // the images embed becomes the media sub-embed
		}
	} else {
		post.Embed = quoteEmbed
	}
}
```

- [ ] **Step 2: Build and verify**

Run: `go build ./...`

- [ ] **Step 3: Commit**

```bash
git add internal/bsky/poster.go
git commit -m "bsky: add outbound quote post support via q-tag → embed.record"
```

---

## Task 5: Content Warnings (All Directions)

**Files:**
- Modify: `internal/bsky/transmute.go` (outbound)
- Modify: `internal/bsky/poster.go` (outbound)
- Modify: `internal/bsky/poller.go` (inbound)

- [ ] **Step 1: Outbound NIP-36 → Bluesky self-labels**

In `internal/bsky/transmute.go`, add:

```go
// BuildSelfLabels creates Bluesky self-labels from a Nostr content-warning tag.
func BuildSelfLabels(event *nostr.Event) *SelfLabels {
	for _, tag := range event.Tags {
		if len(tag) >= 1 && tag[0] == "content-warning" {
			return &SelfLabels{
				Type:   "com.atproto.label.defs#selfLabels",
				Values: []SelfLabel{{Val: "!warn"}},
			}
		}
	}
	return nil
}
```

In `poster.go` `postNote`, after image/quote handling:

```go
if labels := BuildSelfLabels(event); labels != nil {
	post.Labels = labels
}
```

- [ ] **Step 2: Inbound Bluesky self-labels → NIP-36 content-warning tag**

In `internal/bsky/poller.go`, add a helper:

```go
// extractContentWarning checks for self-labels that indicate a content warning.
func extractContentWarning(record map[string]interface{}) string {
	labels, ok := record["labels"].(map[string]interface{})
	if !ok {
		return ""
	}
	values, ok := labels["values"].([]interface{})
	if !ok {
		return ""
	}
	for _, v := range values {
		if m, ok := v.(map[string]interface{}); ok {
			if val, _ := m["val"].(string); val == "!warn" || val == "sexual" || val == "nudity" || val == "graphic-media" {
				return val
			}
		}
	}
	return ""
}
```

Then in `bridgePost` and `bridgeReply`, add the content warning to the NormalizedPost:

```go
np.ContentWarning = extractContentWarning(record)
```

(The `bridge.NormalizedPost` already has a `ContentWarning` field, and `bridge.BuildKind1Event` already handles it by adding a `["content-warning", "..."]` tag.)

- [ ] **Step 3: Build and verify**

Run: `go build ./...`

- [ ] **Step 4: Commit**

```bash
git add internal/bsky/transmute.go internal/bsky/poster.go internal/bsky/poller.go
git commit -m "bsky: add content warning support via NIP-36 ↔ Bluesky self-labels"
```

---

## Task 6: Mention Facets (Outbound: Nostr → Bluesky)

**Files:**
- Modify: `internal/bsky/transmute.go`
- Modify: `internal/bsky/poster.go`

- [ ] **Step 1: Add DID resolver callback to buildFacets**

Change `buildFacets` signature to accept an optional DID resolver:

```go
func buildFacets(text string, resolveDID func(pubkey string) (string, bool)) []Facet
```

After the existing URL and hashtag facet loops, add mention detection. Since Nostr posts may contain `nostr:npub1...` references in the text, find those and resolve to DIDs:

```go
// Mention facets: resolve nostr: URIs in text to DIDs
if resolveDID != nil {
	mentionRe := regexp.MustCompile(`nostr:npub1[a-z0-9]+`)
	for _, loc := range mentionRe.FindAllStringIndex(text, -1) {
		bech32 := text[loc[0]+6:loc[1]] // strip "nostr:"
		prefix, val, err := nip19.Decode(bech32)
		if err != nil || prefix != "npub" {
			continue
		}
		pubkey, _ := val.(string)
		if pubkey == "" {
			continue
		}
		if did, ok := resolveDID(pubkey); ok {
			facets = append(facets, Facet{
				Index: ByteSlice{ByteStart: loc[0], ByteEnd: loc[1]},
				Features: []FacetFeature{{
					Type: "app.bsky.richtext.facet#mention",
					DID:  did,
				}},
			})
		}
	}
}
```

- [ ] **Step 2: Update callers of buildFacets**

In `NostrNoteToFeedPost`, change the call:

```go
// Before: post.Facets = buildFacets(text)
// After:  post.Facets = buildFacets(text, nil)  // or pass the resolver when available
```

In `poster.go` `postNote`, pass a DID resolver that looks up actor_keys:

```go
// The poster doesn't have direct access to actor_keys, so pass nil for now.
// The DID resolution requires Store access which postNote doesn't have.
// Alternative: add a ResolveDID callback to the Poster struct.
```

Actually, the simplest approach: add a `ResolveDID func(pubkey string) (string, bool)` field to the Poster struct, wired from main.go via the actor_keys table. For now, pass nil to maintain backward compatibility.

- [ ] **Step 3: Build and verify**

Run: `go build ./...`

- [ ] **Step 4: Commit**

```bash
git add internal/bsky/transmute.go internal/bsky/poster.go
git commit -m "bsky: add outbound mention facet support for nostr: URI references"
```

---

## Task 7: Mention Facets (Inbound: Bluesky → Nostr)

**Files:**
- Modify: `internal/bsky/poller.go`

- [ ] **Step 1: Add mention extraction from Bluesky facets**

Add to poller.go:

```go
// extractMentionDIDs extracts DIDs from Bluesky mention facets.
func extractMentionDIDs(record map[string]interface{}) []string {
	facets, ok := record["facets"].([]interface{})
	if !ok {
		return nil
	}
	var dids []string
	seen := make(map[string]bool)
	for _, f := range facets {
		fm, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		features, ok := fm["features"].([]interface{})
		if !ok {
			continue
		}
		for _, feat := range features {
			featMap, ok := feat.(map[string]interface{})
			if !ok {
				continue
			}
			if t, _ := featMap["$type"].(string); t == "app.bsky.richtext.facet#mention" {
				if did, _ := featMap["did"].(string); did != "" && !seen[did] {
					dids = append(dids, did)
					seen[did] = true
				}
			}
		}
	}
	return dids
}
```

- [ ] **Step 2: Wire mention DIDs to p-tags in bridgePost/bridgeReply**

In `bridgePost` and `bridgeReply`, after extracting content:

```go
// Resolve Bluesky mention DIDs to Nostr pubkeys for p-tags
mentionDIDs := extractMentionDIDs(record)
for _, did := range mentionDIDs {
	// Look up derived pubkey for this DID in actor_keys
	if pubkey, ok := p.Signer.PublicKey(did); ok {
		np.MentionPubkeys = append(np.MentionPubkeys, pubkey)
	}
}
```

Note: `p.Signer.PublicKey(did)` derives the pubkey for a DID the same way it does for AP actor URLs — the derived key is deterministic from the DID string. This requires the Signer to have seen this DID before (have it cached or in actor_keys). If not found, the mention is silently skipped (best-effort).

- [ ] **Step 3: Build and verify**

Run: `go build ./...`

- [ ] **Step 4: Commit**

```bash
git add internal/bsky/poller.go
git commit -m "bsky: extract Bluesky mention facets → Nostr p-tags"
```

---

## Task 8: NIP-05 for Bridged Bluesky Authors

**Files:**
- Modify: `internal/server/server.go`

- [ ] **Step 1: Extend NIP-05 handler**

Find the `handleNIP05` function. Currently it only serves the local user. Extend it to also serve bridged Bluesky authors:

After the existing local-user check, add:

```go
// Check if name matches a bridged Bluesky author (format: handle.bsky.social)
if pubkey, ok := s.resolveBskyHandle(name); ok {
	// Return NIP-05 response for the bridged author
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"names": map[string]string{
			name: pubkey,
		},
	})
	return
}
```

The `resolveBskyHandle` method should already exist — it looks up actor_keys for a Bluesky handle. If it doesn't exist, add one that queries actor_keys WHERE ap_actor_url matches the handle pattern.

- [ ] **Step 2: Build and verify**

Run: `go build ./...`

- [ ] **Step 3: Commit**

```bash
git add internal/server/server.go
git commit -m "server: extend NIP-05 to serve bridged Bluesky authors"
```

---

## Task 9: "View Original" Reference Tags

**Files:**
- Modify: `internal/bsky/poller.go` (Bluesky → Nostr)
- Modify: `internal/ap/handler.go` (AP → Nostr)

- [ ] **Step 1: Add r-tag in Bluesky poller**

In `bridgePost` and `bridgeReply`, after building the NormalizedPost but before calling `bridge.BuildKind1Event`, the `SourceURL` field should already be set from `atURIToHTTPS(post.URI)`. This gets added as an r-tag by `BuildKind1Event`. Verify this is the case — if `BuildKind1Event` doesn't add an r-tag from SourceURL, add it manually to the event tags after building:

```go
if np.SourceURL != "" {
	event.Tags = append(event.Tags, nostr.Tag{"r", np.SourceURL})
}
```

- [ ] **Step 2: Add r-tag in AP handler**

In `noteToEvent` in `internal/ap/handler.go`, after building the event, add an r-tag from the Note's human-readable URL:

```go
if note.URL != "" {
	event.Tags = append(event.Tags, nostr.Tag{"r", note.URL})
} else if note.ID != "" {
	event.Tags = append(event.Tags, nostr.Tag{"r", note.ID})
}
```

- [ ] **Step 3: Build and verify**

Run: `go build ./...`

- [ ] **Step 4: Commit**

```bash
git add internal/bsky/poller.go internal/ap/handler.go
git commit -m "bridge: add r-tag 'View Original' reference for bridged posts"
```

---

## Task 10: Transmute Test Suite

**Files:**
- Create: `internal/bsky/transmute_test.go`
- Create: `internal/ap/transmute_test.go`

- [ ] **Step 1: Write Bluesky transmute tests**

Create `internal/bsky/transmute_test.go` with table-driven tests:

```go
package bsky

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func TestNostrNoteToFeedPost(t *testing.T) {
	tests := []struct {
		name        string
		event       *nostr.Event
		wantText    string
		wantFacets  int
		wantReply   bool
		wantErr     bool
	}{
		{
			name: "simple text post",
			event: &nostr.Event{
				Kind:    1,
				Content: "Hello world",
				Tags:    nostr.Tags{},
			},
			wantText:   "Hello world",
			wantFacets: 0,
		},
		{
			name: "post with URL",
			event: &nostr.Event{
				Kind:    1,
				Content: "Check https://example.com",
				Tags:    nostr.Tags{},
			},
			wantText:   "Check https://example.com",
			wantFacets: 1,
		},
		{
			name: "post with hashtag",
			event: &nostr.Event{
				Kind:    1,
				Content: "Hello #nostr",
				Tags:    nostr.Tags{{"t", "nostr"}},
			},
			wantText:   "Hello #nostr",
			wantFacets: 2, // URL facets (0) + hashtag facets (1) — verify exact count
		},
		{
			name: "truncation at 300 graphemes",
			event: &nostr.Event{
				Kind:    1,
				Content: string(make([]byte, 500)), // 500 null bytes
				Tags:    nostr.Tags{},
			},
			// Truncated to 300 graphemes with "…" suffix
		},
		{
			name: "empty content",
			event: &nostr.Event{
				Kind:    1,
				Content: "",
				Tags:    nostr.Tags{},
			},
			wantText: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.event.CreatedAt = nostr.Now()
			post, err := NostrNoteToFeedPost(tc.event, "https://njump.me", nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if tc.wantText != "" && post.Text != tc.wantText {
				t.Errorf("text = %q, want %q", post.Text, tc.wantText)
			}
		})
	}
}

func TestBuildFacets(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantCount  int
		wantTypes  []string
	}{
		{"no facets", "plain text", 0, nil},
		{"single URL", "visit https://example.com today", 1, []string{"app.bsky.richtext.facet#link"}},
		{"single hashtag", "hello #nostr", 1, []string{"app.bsky.richtext.facet#tag"}},
		{"URL and hashtag", "visit https://x.com #nostr", 2, nil},
		{"multiple URLs", "a https://a.com b https://b.com", 2, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facets := buildFacets(tc.text, nil)
			if len(facets) != tc.wantCount {
				t.Errorf("facet count = %d, want %d", len(facets), tc.wantCount)
			}
		})
	}
}

func TestExtractContentFromRecord(t *testing.T) {
	tests := []struct {
		name    string
		record  map[string]interface{}
		want    string
	}{
		{
			name:   "plain text",
			record: map[string]interface{}{"text": "hello"},
			want:   "hello",
		},
		{
			name: "text with hidden facet link",
			record: map[string]interface{}{
				"text": "click here",
				"facets": []interface{}{
					map[string]interface{}{
						"features": []interface{}{
							map[string]interface{}{
								"$type": "app.bsky.richtext.facet#link",
								"uri":   "https://example.com",
							},
						},
					},
				},
			},
			want: "click here\n\nhttps://example.com",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractContentFromRecord(tc.record)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Write AP transmute tests**

Create `internal/ap/transmute_test.go` — test `ToNote` and `ToActor` with various inputs. This tests that content warnings, mentions, imeta tags, and proxy metadata are correctly mapped.

(The exact test cases depend on the `ToNote` and `ToActor` signatures which accept `*nostr.Event` and `*TransmuteContext`. Create a test `TransmuteContext` with a mock `GetAPIDForObject` callback.)

```go
package ap

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func testTC() *TransmuteContext {
	return &TransmuteContext{
		LocalDomain:   "https://bridge.example.com",
		LocalActorURL: "https://bridge.example.com/users/alice",
		PublicKeyPem:  "test-key-pem",
		GetAPIDForObject: func(nostrID string) (string, bool) {
			return "https://bridge.example.com/objects/" + nostrID, true
		},
	}
}

func TestToNote(t *testing.T) {
	tests := []struct {
		name           string
		event          *nostr.Event
		wantContent    bool
		wantCW         bool
		wantAttachment int
	}{
		{
			name: "simple note",
			event: &nostr.Event{
				Kind:    1,
				Content: "Hello AP world",
				Tags:    nostr.Tags{},
			},
			wantContent: true,
		},
		{
			name: "note with content warning",
			event: &nostr.Event{
				Kind:    1,
				Content: "Spoiler content",
				Tags:    nostr.Tags{{"content-warning", "spoilers"}},
			},
			wantContent: true,
			wantCW:      true,
		},
		{
			name: "note with imeta image",
			event: &nostr.Event{
				Kind:    1,
				Content: "Photo post",
				Tags: nostr.Tags{
					{"imeta", "url https://img.example.com/photo.jpg", "m image/jpeg", "alt A nice photo"},
				},
			},
			wantContent:    true,
			wantAttachment: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.event.CreatedAt = nostr.Now()
			tc.event.PubKey = "0000000000000000000000000000000000000000000000000000000000000001"
			note := ToNote(tc.event, testTC())
			if note == nil {
				t.Fatal("ToNote returned nil")
			}
			if tc.wantCW && note.Summary == "" {
				t.Error("expected content warning summary, got empty")
			}
			if tc.wantAttachment > 0 && len(note.Attachment) != tc.wantAttachment {
				t.Errorf("attachments = %d, want %d", len(note.Attachment), tc.wantAttachment)
			}
		})
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/bsky/... ./internal/ap/... -v`

- [ ] **Step 4: Commit**

```bash
git add internal/bsky/transmute_test.go internal/ap/transmute_test.go
git commit -m "test: add table-driven transmute tests for AP and Bluesky conversions"
```

---

## Task 11: Update CLAUDE.md

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Document Stage 2 changes**

Add to the relevant sections:
- Note that outbound Bluesky images are supported via blob upload
- Note quote post embed support
- Note bidirectional content warning support
- Note mention facet support (best-effort)
- Note NIP-05 for bridged Bluesky authors
- Note r-tag "View Original" references

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: document Stage 2 protocol fidelity features in CLAUDE.md"
```
