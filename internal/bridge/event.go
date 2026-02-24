// Package bridge provides protocol-agnostic types and helpers shared by the
// ActivityPub and Bluesky inbound bridge parsers. It has no local imports so
// neither internal/ap nor internal/bsky create a dependency cycle.
package bridge

import (
	"fmt"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

// ImageInfo holds normalised media attachment metadata for NIP-94 imeta tags.
// Used by both the ActivityPub and Bluesky bridge parsers.
type ImageInfo struct {
	URL      string
	Alt      string
	MimeType string
	Blurhash string
	Width    int
	Height   int
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

	// Threading (NIP-10 positional convention).
	// If RootEventID is empty or equals ReplyToEventID, a single e-tag is emitted.
	ReplyToEventID string
	RootEventID    string
	RelayHint      string // optional relay URL used in e/p/q tags

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

	// Thread e-tags (NIP-10 positional convention).
	if post.ReplyToEventID != "" {
		root := post.RootEventID
		if root == "" || root == post.ReplyToEventID {
			// Single parent: one positional e-tag.
			if post.RelayHint != "" {
				tags = append(tags, nostr.Tag{"e", post.ReplyToEventID, post.RelayHint})
			} else {
				tags = append(tags, nostr.Tag{"e", post.ReplyToEventID})
			}
		} else {
			// Multi-level thread: root first, direct parent last.
			if post.RelayHint != "" {
				tags = append(tags, nostr.Tag{"e", root, post.RelayHint})
				tags = append(tags, nostr.Tag{"e", post.ReplyToEventID, post.RelayHint})
			} else {
				tags = append(tags, nostr.Tag{"e", root})
				tags = append(tags, nostr.Tag{"e", post.ReplyToEventID})
			}
		}
	}

	// Mention p-tags.
	for _, pk := range post.MentionPubkeys {
		if post.RelayHint != "" {
			tags = append(tags, nostr.Tag{"p", pk, post.RelayHint})
		} else {
			tags = append(tags, nostr.Tag{"p", pk})
		}
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

	// Source link attribution: full URL appended to content and stored in r-tag.
	if post.ShowSourceLink && post.SourceURL != "" && !strings.Contains(content, post.SourceURL) {
		tags = append(tags, nostr.Tag{"r", post.SourceURL})
		content += "\n\n🔗 " + post.SourceURL
	}

	return &nostr.Event{
		Kind:      1,
		Content:   content,
		CreatedAt: post.CreatedAt,
		Tags:      tags,
	}
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
	return nostr.Tag(parts)
}
