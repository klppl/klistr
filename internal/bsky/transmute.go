package bsky

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/rivo/uniseg"

	"github.com/klppl/klistr/internal/bridge"
)

const (
	maxGraphemes   = 300
	feedPostType   = "app.bsky.feed.post"
	likeType       = "app.bsky.feed.like"
	repostType     = "app.bsky.feed.repost"
	facetLinkType  = "app.bsky.richtext.facet#link"
	facetTagType   = "app.bsky.richtext.facet#tag"
)

var (
	urlRegex     = regexp.MustCompile(`https?://[^\s<>"{}|\\^` + "`" + `\[\]]+`)
	hashtagRegex = regexp.MustCompile(`(?:^|[^\w])#([a-zA-Z][a-zA-Z0-9_]*)`)
)

// NostrNoteToFeedPost converts a Nostr kind-1 event to a Bluesky FeedPost.
// getATURI resolves a Nostr event ID to an AT URI for threading (may be nil).
func NostrNoteToFeedPost(event *nostr.Event, externalBaseURL string, getATURI func(nostrID string) (string, bool)) (*FeedPost, error) {
	text := event.Content

	// Truncate to 300 graphemes, appending an njump link if truncated.
	var truncated bool
	text, truncated = truncateGraphemes(text, maxGraphemes)
	if truncated {
		suffix := "\n…\n" + strings.TrimRight(externalBaseURL, "/") + "/" + event.ID
		// Make room for the suffix.
		available := maxGraphemes - graphemeCount(suffix)
		if available < 0 {
			available = 0
		}
		text, _ = truncateGraphemes(event.Content, available)
		text += suffix
	}

	post := &FeedPost{
		Type:      feedPostType,
		Text:      text,
		CreatedAt: event.CreatedAt.Time().UTC().Format(time.RFC3339),
		Langs:     []string{"en"},
	}

	// Build facets for URLs and hashtags.
	post.Facets = buildFacets(text)

	// Resolve reply threading via e-tags.
	if getATURI != nil {
		post.Reply = buildReply(event, getATURI)
	}

	return post, nil
}

// buildReply constructs a Reply struct from e-tags if this note is a reply.
// Bluesky requires both root and parent refs.
func buildReply(event *nostr.Event, getATURI func(string) (string, bool)) *Reply {
	// Collect e-tags with their markers.
	type eTag struct {
		id     string
		marker string
	}
	var eTags []eTag
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			marker := ""
			if len(tag) >= 4 {
				marker = tag[3]
			}
			eTags = append(eTags, eTag{id: tag[1], marker: marker})
		}
	}
	if len(eTags) == 0 {
		return nil
	}

	// Find root and reply markers (NIP-10 convention).
	var rootID, replyID string
	for _, t := range eTags {
		switch t.marker {
		case "root":
			rootID = t.id
		case "reply":
			replyID = t.id
		}
	}
	// Deprecated positional tagging: first = root, last = reply.
	if rootID == "" && replyID == "" {
		if len(eTags) == 1 {
			rootID = eTags[0].id
			replyID = eTags[0].id
		} else {
			rootID = eTags[0].id
			replyID = eTags[len(eTags)-1].id
		}
	}
	if rootID == "" {
		rootID = replyID
	}
	if replyID == "" {
		replyID = rootID
	}
	if rootID == "" {
		return nil
	}

	rootURI, rootOK := getATURI(rootID)
	parentURI, parentOK := getATURI(replyID)
	if !rootOK || !parentOK {
		// Can't build a valid reply ref without AT URIs.
		return nil
	}

	return &Reply{
		Root:   Ref{URI: rootURI},
		Parent: Ref{URI: parentURI},
	}
}

// buildFacets scans text for URLs and hashtags and returns rich-text facets.
// Byte offsets are computed over the UTF-8 encoded text, as required by AT Protocol.
func buildFacets(text string) []Facet {
	var facets []Facet

	// URLs.
	for _, loc := range urlRegex.FindAllStringIndex(text, -1) {
		uri := text[loc[0]:loc[1]]
		facets = append(facets, Facet{
			Index: ByteSlice{ByteStart: loc[0], ByteEnd: loc[1]},
			Features: []FacetFeature{{
				Type: facetLinkType,
				URI:  uri,
			}},
		})
	}

	// Hashtags.
	for _, loc := range hashtagRegex.FindAllStringSubmatchIndex(text, -1) {
		// loc[0]:loc[1] is full match; loc[2]:loc[3] is capture group (tag name without #).
		if len(loc) < 4 {
			continue
		}
		// Find the '#' character preceding the tag name.
		hashStart := strings.LastIndex(text[:loc[2]], "#")
		if hashStart < 0 {
			continue
		}
		tagName := text[loc[2]:loc[3]]
		facets = append(facets, Facet{
			Index: ByteSlice{ByteStart: hashStart, ByteEnd: loc[3]},
			Features: []FacetFeature{{
				Type: facetTagType,
				Tag:  tagName,
			}},
		})
	}

	return facets
}

// truncateGraphemes truncates s to at most n Unicode grapheme clusters.
// Returns the (possibly truncated) string and whether truncation occurred.
func truncateGraphemes(s string, n int) (string, bool) {
	gr := uniseg.NewGraphemes(s)
	count := 0
	var endPos int
	for gr.Next() {
		count++
		_, endPos = gr.Positions()
		if count == n {
			// Check if there is remaining text beyond the nth grapheme.
			if gr.Next() {
				return s[:endPos], true
			}
			return s, false
		}
	}
	return s, false
}

// graphemeCount returns the number of Unicode grapheme clusters in s.
func graphemeCount(s string) int {
	return uniseg.GraphemeClusterCount(s)
}

// ─── Bluesky → Nostr ─────────────────────────────────────────────────────────

// NotificationToNostrEvent converts a Bluesky notification to a Nostr event.
// Returns nil for notification types that don't map to Nostr events (e.g. "follow").
// The returned event is unsigned; call SignAsUser before publishing.
func NotificationToNostrEvent(n *Notification, localPubKey string) (*nostr.Event, error) {
	proxyTag := nostr.Tag{"proxy", n.URI, "atproto"}

	switch n.Reason {
	case "like":
		// Bluesky like → Nostr kind-7 "+" reaction.
		// We point the e-tag at the URI (used as a reference; nostr ID lookup is done by caller).
		event := &nostr.Event{
			Kind:      7,
			Content:   "+",
			CreatedAt: nostr.Now(),
			Tags:      nostr.Tags{{"e", n.URI}, proxyTag},
			PubKey:    localPubKey,
		}
		return event, nil

	case "repost":
		// Bluesky repost → Nostr kind-6.
		event := &nostr.Event{
			Kind:      6,
			Content:   "",
			CreatedAt: nostr.Now(),
			Tags:      nostr.Tags{{"e", n.URI, "", "mention"}, proxyTag},
			PubKey:    localPubKey,
		}
		return event, nil

	case "follow":
		// Handled via DM notification in poller; no Nostr event created here.
		return nil, nil

	default:
		return nil, nil
	}
}

// atURIToHTTPS converts an AT URI (at://did/collection/rkey) to a bsky.app URL.
func atURIToHTTPS(uri string) string {
	// at://did.plc.xxx/app.bsky.feed.post/rkey
	if !strings.HasPrefix(uri, "at://") {
		return uri
	}
	rest := strings.TrimPrefix(uri, "at://")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 {
		return "https://bsky.app"
	}
	did := parts[0]
	// parts[1] is collection (e.g. app.bsky.feed.post), parts[2] is rkey
	rkey := parts[2]
	return fmt.Sprintf("https://bsky.app/profile/%s/post/%s", did, rkey)
}

// extractReplyRefs returns the parent and root AT URIs from a Bluesky reply
// record's reply reference block (reply.parent.uri / reply.root.uri).
// Returns empty strings if the record is not a reply or the fields are absent.
func extractReplyRefs(n *Notification) (parentURI, rootURI string) {
	if n.Record == nil {
		return
	}
	m, ok := n.Record.(map[string]interface{})
	if !ok {
		return
	}
	reply, ok := m["reply"].(map[string]interface{})
	if !ok {
		return
	}
	if parent, ok := reply["parent"].(map[string]interface{}); ok {
		parentURI, _ = parent["uri"].(string)
	}
	if root, ok := reply["root"].(map[string]interface{}); ok {
		rootURI, _ = root["uri"].(string)
	}
	return
}

// extractContentFromRecord extracts bridgeable text content from a Bluesky
// post record map. It reads the plain "text" field and then appends any link
// URIs that only appear in rich-text facets (anchor-text links) or in an
// external embed (link card) but are absent from the plain text. This ensures
// that "click here → https://…" style links are not silently dropped.
func extractContentFromRecord(record map[string]interface{}) string {
	if record == nil {
		return ""
	}
	text, _ := record["text"].(string)

	seen := make(map[string]bool)
	var extraLinks []string

	addLink := func(uri string) {
		if uri == "" || strings.Contains(text, uri) || seen[uri] {
			return
		}
		seen[uri] = true
		extraLinks = append(extraLinks, uri)
	}

	// Rich-text facet links (anchor text whose URL is only in the facets).
	if facets, ok := record["facets"].([]interface{}); ok {
		for _, f := range facets {
			facet, ok := f.(map[string]interface{})
			if !ok {
				continue
			}
			features, ok := facet["features"].([]interface{})
			if !ok {
				continue
			}
			for _, feat := range features {
				feature, ok := feat.(map[string]interface{})
				if !ok {
					continue
				}
				if ftype, _ := feature["$type"].(string); ftype == facetLinkType {
					uri, _ := feature["uri"].(string)
					addLink(uri)
				}
			}
		}
	}

	// External embed / link card.
	if embed, ok := record["embed"].(map[string]interface{}); ok {
		if embedType, _ := embed["$type"].(string); embedType == "app.bsky.embed.external" {
			if ext, ok := embed["external"].(map[string]interface{}); ok {
				uri, _ := ext["uri"].(string)
				addLink(uri)
			}
		}
	}

	if len(extraLinks) == 0 {
		return text
	}
	return text + "\n\n" + strings.Join(extraLinks, "\n")
}

// extractNotifText attempts to pull the text from a notification record.
// Prefer extractContentFromRecord when the record map is already available.
func extractNotifText(n *Notification) string {
	if n.Record == nil {
		return ""
	}
	m, ok := n.Record.(map[string]interface{})
	if !ok {
		return ""
	}
	return extractContentFromRecord(m)
}

// ─── Image extraction (Bluesky → Nostr) ──────────────────────────────────────

// blobToCDNURL constructs a Bluesky CDN URL for a blob.
// authorDID is the post author's DID; cid is the blob's $link value.
func blobToCDNURL(authorDID, cid, mimeType string) string {
	format := "jpeg"
	if parts := strings.SplitN(mimeType, "/", 2); len(parts) == 2 && parts[1] != "" {
		format = parts[1]
	}
	return fmt.Sprintf("https://cdn.bsky.app/img/feed_fullsize/plain/%s/%s@%s", authorDID, cid, format)
}

// extractImagesFromRecord returns bridge.ImageInfo for every image or video in
// an app.bsky.embed.images or app.bsky.embed.video embed block. authorDID is
// required to construct CDN blob URLs. Returns nil if the record has no
// image/video embed.
func extractImagesFromRecord(record map[string]interface{}, authorDID string) []bridge.ImageInfo {
	if record == nil || authorDID == "" {
		return nil
	}
	embed, ok := record["embed"].(map[string]interface{})
	if !ok {
		return nil
	}
	embedType, _ := embed["$type"].(string)

	switch embedType {
	case "app.bsky.embed.images":
		images, ok := embed["images"].([]interface{})
		if !ok {
			return nil
		}

		var result []bridge.ImageInfo
		for _, img := range images {
			image, ok := img.(map[string]interface{})
			if !ok {
				continue
			}
			blob, ok := image["image"].(map[string]interface{})
			if !ok {
				continue
			}
			ref, ok := blob["ref"].(map[string]interface{})
			if !ok {
				continue
			}
			cid, _ := ref["$link"].(string)
			if cid == "" {
				continue
			}
			mimeType, _ := blob["mimeType"].(string)
			alt, _ := image["alt"].(string)

			var width, height int
			if ar, ok := image["aspectRatio"].(map[string]interface{}); ok {
				if w, ok := ar["width"].(float64); ok {
					width = int(w)
				}
				if h, ok := ar["height"].(float64); ok {
					height = int(h)
				}
			}

			result = append(result, bridge.ImageInfo{
				URL:      blobToCDNURL(authorDID, cid, mimeType),
				Alt:      alt,
				MimeType: mimeType,
				Width:    width,
				Height:   height,
			})
		}
		return result

	case "app.bsky.embed.video":
		video, ok := embed["video"].(map[string]interface{})
		if !ok {
			return nil
		}
		ref, ok := video["ref"].(map[string]interface{})
		if !ok {
			return nil
		}
		cid, _ := ref["$link"].(string)
		if cid == "" {
			return nil
		}
		mimeType, _ := video["mimeType"].(string)
		if mimeType == "" {
			mimeType = "video/mp4"
		}
		alt, _ := embed["alt"].(string)

		var width, height int
		if ar, ok := embed["aspectRatio"].(map[string]interface{}); ok {
			if w, ok := ar["width"].(float64); ok {
				width = int(w)
			}
			if h, ok := ar["height"].(float64); ok {
				height = int(h)
			}
		}

		// Bluesky serves video as HLS; the playlist URL is publicly accessible.
		playlistURL := fmt.Sprintf("https://video.bsky.app/watch/%s/%s/playlist.m3u8", authorDID, cid)
		return []bridge.ImageInfo{{
			URL:      playlistURL,
			Alt:      alt,
			MimeType: mimeType,
			Width:    width,
			Height:   height,
		}}
	}

	return nil
}

// ─── Hashtag + quote extraction (Bluesky → Nostr) ────────────────────────────

// extractHashtagsFromRecord returns hashtag tag names from Bluesky richtext
// facets (app.bsky.richtext.facet#tag). Duplicates are suppressed.
func extractHashtagsFromRecord(record map[string]interface{}) []string {
	if record == nil {
		return nil
	}
	facets, ok := record["facets"].([]interface{})
	if !ok {
		return nil
	}
	seen := make(map[string]bool)
	var tags []string
	for _, f := range facets {
		facet, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		features, ok := facet["features"].([]interface{})
		if !ok {
			continue
		}
		for _, feat := range features {
			feature, ok := feat.(map[string]interface{})
			if !ok {
				continue
			}
			if ftype, _ := feature["$type"].(string); ftype == facetTagType {
				tag, _ := feature["tag"].(string)
				if tag != "" && !seen[tag] {
					seen[tag] = true
					tags = append(tags, tag)
				}
			}
		}
	}
	return tags
}

// extractQuoteURI returns the AT URI of a quoted post from embed.record or
// embed.recordWithMedia. Returns empty string if no quote embed is present.
func extractQuoteURI(record map[string]interface{}) string {
	if record == nil {
		return ""
	}
	embed, ok := record["embed"].(map[string]interface{})
	if !ok {
		return ""
	}
	switch embedType, _ := embed["$type"].(string); embedType {
	case "app.bsky.embed.record":
		if rec, ok := embed["record"].(map[string]interface{}); ok {
			uri, _ := rec["uri"].(string)
			return uri
		}
	case "app.bsky.embed.recordWithMedia":
		if rec, ok := embed["record"].(map[string]interface{}); ok {
			if inner, ok := rec["record"].(map[string]interface{}); ok {
				uri, _ := inner["uri"].(string)
				return uri
			}
		}
	}
	return ""
}

// RKeyFromURI extracts the rkey from an AT URI (at://did/collection/rkey).
func RKeyFromURI(uri string) string {
	parts := strings.Split(uri, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// CollectionFromURI extracts the collection (e.g. "app.bsky.feed.post") from an AT URI.
func CollectionFromURI(uri string) string {
	// at://did/collection/rkey
	trimmed := strings.TrimPrefix(uri, "at://")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}
