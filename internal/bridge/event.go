// Package bridge provides protocol-agnostic types and helpers shared by the
// ActivityPub and Bluesky inbound bridge parsers. It has no local imports so
// neither internal/ap nor internal/bsky create a dependency cycle.
package bridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

// ImageInfo holds normalised media attachment metadata for NIP-94 imeta tags.
// Used by both the ActivityPub and Bluesky bridge parsers.
type ImageInfo struct {
	URL         string
	FallbackURL string // optional alternative URL (NIP-94 "fallback" field)
	Alt         string
	MimeType    string
	Blurhash    string
	Width       int
	Height      int
}

// NormalizedPost is a protocol-agnostic intermediate representation of an
// inbound post. Both the AP and Bluesky parsers populate this struct; a single
// BuildKind1Event call then converts it to an unsigned Nostr kind-1 event,
// ensuring consistent behaviour (imeta tags, source attribution, hashtags,
// quotes) across both bridges.
type NormalizedPost struct {
	Content   string
	CreatedAt nostr.Timestamp

	// Media attachments.
	Images []ImageInfo

	// Threading (NIP-10 marker convention).
	// If RootEventID is empty or equals ReplyToEventID, a single "root"-marked
	// e-tag is emitted (a direct reply to the thread root).
	ReplyToEventID string
	RootEventID    string
	RelayHint      string // optional relay URL used in e/p/q tags

	// Author pubkeys of the events being replied to. NIP-10 requires a reply
	// to p-tag the author it responds to (so they are notified and clients can
	// thread correctly). Optional; emitted only when ReplyToEventID is set.
	ReplyToPubkey string
	RootPubkey    string

	// References.
	MentionPubkeys []string // → p-tags
	QuoteEventID   string   // → q-tag

	// Metadata.
	Hashtags       []string // → t-tags
	ContentWarning string   // → content-warning tag

	// Source attribution (SHOW_SOURCE_LINK).
	// Full URL goes into an r-tag; only the bare hostname goes into content
	// so it does not trigger an embed card that overshadows shared links.
	SourceURL      string
	ShowSourceLink bool

	// NIP-40 expiration (unix timestamp). Zero means no expiry.
	ExpiresAt int64

	// Protocol identity.
	ProxyID       string // proxy tag value (AP note ID or AT URI)
	ProxyProtocol string // "activitypub" or "atproto"
}

// BuildKind1Event converts a NormalizedPost into an unsigned Nostr kind-1
// event. The caller is responsible for signing before publishing.
func BuildKind1Event(post NormalizedPost) *nostr.Event {
	content := post.Content
	tags := nostr.Tags{}

	// Proxy tag first so loop-prevention checks on downstream relays fire early.
	if post.ProxyID != "" {
		tags = append(tags, nostr.Tag{"proxy", post.ProxyID, post.ProxyProtocol})
	}

	// Thread e-tags (NIP-10 marker convention: preferred over deprecated positional style).
	// relay hint may be empty string; markers are always set.
	if post.ReplyToEventID != "" {
		relay := post.RelayHint
		root := post.RootEventID
		if root == "" || root == post.ReplyToEventID {
			// NIP-10: a direct reply to the thread root has a single e-tag
			// marked "root" (the root is also the event being responded to).
			tags = append(tags, nostr.Tag{"e", post.ReplyToEventID, relay, "root"})
		} else {
			// Multi-level thread: root marker first, reply marker last.
			tags = append(tags, nostr.Tag{"e", root, relay, "root"})
			tags = append(tags, nostr.Tag{"e", post.ReplyToEventID, relay, "reply"})
		}
	}

	// Mention p-tags. NIP-10 requires a reply to p-tag the author(s) it responds
	// to, so the parent (and root) author pubkeys are emitted first and deduped
	// against explicit mentions. Without these the replied-to author is never
	// notified on Nostr and many clients fail to nest the reply.
	seenP := make(map[string]bool)
	addPTag := func(pk string) {
		if pk == "" || seenP[pk] {
			return
		}
		seenP[pk] = true
		if post.RelayHint != "" {
			tags = append(tags, nostr.Tag{"p", pk, post.RelayHint})
		} else {
			tags = append(tags, nostr.Tag{"p", pk})
		}
	}
	if post.ReplyToEventID != "" {
		addPTag(post.ReplyToPubkey)
		addPTag(post.RootPubkey)
	}
	for _, pk := range post.MentionPubkeys {
		addPTag(pk)
	}

	// Quote q-tag.
	if post.QuoteEventID != "" {
		if post.RelayHint != "" {
			tags = append(tags, nostr.Tag{"q", post.QuoteEventID, post.RelayHint})
		} else {
			tags = append(tags, nostr.Tag{"q", post.QuoteEventID})
		}
	}

	// Hashtag t-tags.
	for _, ht := range post.Hashtags {
		tags = append(tags, nostr.Tag{"t", ht})
	}

	// Content warning.
	if post.ContentWarning != "" {
		tags = append(tags, nostr.Tag{"content-warning", post.ContentWarning})
	}

	// Image imeta tags + append CDN/media URLs to content.
	for _, img := range post.Images {
		tags = append(tags, buildImeta(img))
		content += "\n\n" + img.URL
	}

	// "View Original" r-tag: always present so Nostr clients can offer a
	// link back to the source post regardless of the ShowSourceLink setting.
	if post.SourceURL != "" {
		tags = append(tags, nostr.Tag{"r", post.SourceURL})
	}

	// Source link attribution: append human-visible link to content when enabled.
	if post.ShowSourceLink && post.SourceURL != "" && !strings.Contains(content, post.SourceURL) {
		content += "\n\n🔗 " + post.SourceURL
	}

	// NIP-40 expiration tag.
	if post.ExpiresAt > 0 {
		tags = append(tags, nostr.Tag{"expiration", fmt.Sprintf("%d", post.ExpiresAt)})
	}

	return &nostr.Event{
		Kind:      1,
		Content:   content,
		CreatedAt: post.CreatedAt,
		Tags:      tags,
	}
}

// BskyProfileMeta holds the fields needed to build a kind-0 Nostr metadata
// event for a bridged Bluesky author. Used by both the Poller and the follow
// management endpoints so Bluesky profiles are rendered consistently.
type BskyProfileMeta struct {
	DisplayName string
	Handle      string
	AvatarURL   string
	BannerURL   string
	Description string
	LocalDomain string // if set, generates NIP-05: <handle>@<localHost>
}

// BuildBskyProfileMeta builds the content field of a kind-0 event for a Bluesky
// profile. Returns a JSON string suitable for use as the .Content of a kind-0.
func BuildBskyProfileMeta(p BskyProfileMeta) string {
	profileURL := "https://bsky.app/profile/" + p.Handle

	name := p.DisplayName
	if name == "" {
		name = p.Handle
	}

	about := profileURL
	if p.Description != "" {
		about = p.Description + "\n\n" + profileURL
	}

	var nip05 string
	if p.LocalDomain != "" {
		localHost := ExtractHost(p.LocalDomain)
		if localHost != "" {
			nip05 = p.Handle + "@" + localHost
		}
	}

	meta := struct {
		Name    string `json:"name"`
		About   string `json:"about"`
		Picture string `json:"picture,omitempty"`
		Banner  string `json:"banner,omitempty"`
		Website string `json:"website"`
		NIP05   string `json:"nip05,omitempty"`
	}{
		Name:    name,
		About:   about,
		Picture: p.AvatarURL,
		Banner:  p.BannerURL,
		Website: profileURL,
		NIP05:   nip05,
	}

	b, err := json.Marshal(meta)
	if err != nil {
		return `{"name":"","about":""}`
	}
	return string(b)
}

// ExtractHost returns the hostname from a URL string
// (e.g. "https://bsky.app/profile/…" → "bsky.app").
// Returns an empty string when the input does not look like a URL.
func ExtractHost(rawURL string) string {
	rest, ok := strings.CutPrefix(rawURL, "https://")
	if !ok {
		rest, ok = strings.CutPrefix(rawURL, "http://")
		if !ok {
			return ""
		}
	}
	host, _, _ := strings.Cut(rest, "/")
	return host
}

// buildImeta constructs a NIP-94 imeta tag from an ImageInfo.
// Fields follow the NIP-94 space-separated key-value format:
//
//	url      — primary media URL (required)
//	m        — MIME type
//	dim      — pixel dimensions (WxH)
//	blurhash — perceptual hash for placeholder rendering
//	alt      — accessibility description
//	fallback — alternate URL when the primary CDN is unreachable
func buildImeta(img ImageInfo) nostr.Tag {
	parts := []string{"imeta", "url " + img.URL}
	if img.MimeType != "" {
		parts = append(parts, "m "+img.MimeType)
	}
	if img.Width > 0 && img.Height > 0 {
		parts = append(parts, fmt.Sprintf("dim %dx%d", img.Width, img.Height))
	}
	if img.Blurhash != "" {
		parts = append(parts, "blurhash "+img.Blurhash)
	}
	if img.Alt != "" {
		parts = append(parts, "alt "+img.Alt)
	}
	if img.FallbackURL != "" && img.FallbackURL != img.URL {
		parts = append(parts, "fallback "+img.FallbackURL)
	}
	return nostr.Tag(parts)
}
