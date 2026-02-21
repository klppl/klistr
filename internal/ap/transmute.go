package ap

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// TransmuteContext provides dependencies for Nostr→AP conversion.
type TransmuteContext struct {
	LocalDomain      string
	LocalActorURL    string // full URL of the local AP actor, e.g. "https://domain.com/users/alice"
	PublicKeyPem     string
	GetAPIDForObject func(nostrID string) (string, bool)
}

// baseURL constructs an absolute URL from a path.
func (tc *TransmuteContext) baseURL(path string) string {
	return strings.TrimRight(tc.LocalDomain, "/") + path
}

// actorURL returns the AP URL for the local actor.
// In the single-user design, all Nostr events in the Nostr→AP direction
// originate from the configured local user.
func (tc *TransmuteContext) actorURL(_ string) string {
	return tc.LocalActorURL
}

// objectURL returns the AP URL for a Nostr event ID.
func (tc *TransmuteContext) objectURL(eventID string) string {
	if apID, ok := tc.GetAPIDForObject(eventID); ok {
		return apID
	}
	return tc.baseURL("/objects/" + eventID)
}

var (
	linebreakRe = regexp.MustCompile(`\n`)
	tagRefRe    = regexp.MustCompile(`\n{0,2}#\[\d+\]$`)
	trailingRe  = regexp.MustCompile(`\s+$`)
	urlRe       = regexp.MustCompile(`https?://[^\s<>"{}|\\^` + "`" + `\[\]]+`)
	hashtagRe   = regexp.MustCompile(`#(\w+)`)
	mentionRe   = regexp.MustCompile(`nostr:(npub|nprofile)[a-z0-9]+`)
)

// NostrDate formats a Unix timestamp as an ISO 8601 string.
func NostrDate(ts nostr.Timestamp) string {
	return time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
}

// ParseNostrDate parses an ISO 8601 date string into a Unix timestamp.
func ParseNostrDate(s string) nostr.Timestamp {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nostr.Now()
	}
	return nostr.Timestamp(t.Unix())
}

// ToActor converts a Nostr kind-0 metadata event to an AP Actor.
func ToActor(event *nostr.Event, tc *TransmuteContext) *Actor {
	meta := parseMetadata(event.Content)
	actorURL := tc.LocalActorURL

	actor := &Actor{
		ID:                actorURL,
		Type:              "Person",
		PreferredUsername: event.PubKey,
		Name:              meta.Name,
		Summary:           linkifyText(meta.About),
		Inbox:             actorURL + "/inbox",
		Outbox:            actorURL + "/outbox",
		Followers:         actorURL + "/followers",
		Following:         actorURL + "/following",
		PublicKey: &PublicKey{
			ID:           actorURL + "#main-key",
			Owner:        actorURL,
			PublicKeyPem: tc.PublicKeyPem,
		},
		Endpoints: &Endpoints{
			SharedInbox: tc.baseURL("/inbox"),
		},
		ProxyOf: []Proxy{toNpubProxy(event)},
	}

	if meta.Picture != "" {
		actor.Icon = &Image{Type: "Image", URL: meta.Picture}
	}
	if meta.Banner != "" {
		actor.Image = &Image{Type: "Image", URL: meta.Banner}
	}

	// Profile fields.
	for _, field := range meta.Fields {
		if len(field) >= 2 {
			actor.Attachment = append(actor.Attachment, PropertyValue{
				Type:  "PropertyValue",
				Name:  field[0],
				Value: linkifyText(field[1]),
			})
		}
	}

	// Emoji tags.
	for _, tag := range event.Tags {
		if len(tag) >= 3 && tag[0] == "emoji" {
			actor.Tag = append(actor.Tag, Emoji{
				Type: "Emoji",
				Name: ":" + tag[1] + ":",
				Icon: &Image{Type: "Image", URL: tag[2]},
			})
		}
	}

	return actor
}

// ToNote converts a Nostr kind-1 text note to an AP Note.
func ToNote(event *nostr.Event, tc *TransmuteContext) *Note {
	content := renderContent(event.Content, event.Tags, tc)

	note := &Note{
		ID:           tc.objectURL(event.ID),
		Type:         "Note",
		AttributedTo: tc.actorURL(event.PubKey),
		Content:      content,
		Published:    NostrDate(event.CreatedAt),
		To:           []string{PublicURI},
		CC:           []string{tc.actorURL(event.PubKey) + "/followers"},
		Generator: &Generator{
			Type: "Application",
			Name: "klistr",
			URL:  "https://github.com/klppl/klistr",
		},
		ProxyOf: []Proxy{toNoteProxy(event)},
	}

	// inReplyTo: find root or reply 'e' tag.
	if replyTag := findReplyTag(event); replyTag != "" {
		note.InReplyTo = tc.objectURL(replyTag)
	}

	// quoteUrl.
	if quoteID := findQuoteID(event); quoteID != "" {
		note.QuoteURL = tc.objectURL(quoteID)
	}

	// Tags: mentions, hashtags, emojis.
	for _, tag := range event.Tags {
		switch {
		case len(tag) >= 2 && tag[0] == "p":
			note.Tag = append(note.Tag, Mention{
				Type: "Mention",
				Href: tc.actorURL(tag[1]),
				Name: "@" + tag[1][:8],
			})
			note.To = append(note.To, tc.actorURL(tag[1]))
		case len(tag) >= 2 && tag[0] == "t":
			note.Tag = append(note.Tag, Hashtag{
				Type: "Hashtag",
				Href: tc.baseURL("/tags/" + tag[1]),
				Name: "#" + tag[1],
			})
		case len(tag) >= 3 && tag[0] == "emoji":
			note.Tag = append(note.Tag, Emoji{
				Type: "Emoji",
				Name: ":" + tag[1] + ":",
				Icon: &Image{Type: "Image", URL: tag[2]},
			})
		case len(tag) >= 2 && tag[0] == "content-warning":
			note.Sensitive = true
			if len(tag) >= 2 {
				note.Summary = tag[1]
			}
		}
	}

	// Media attachments from imeta tags.
	for _, tag := range event.Tags {
		if tag[0] == "imeta" {
			att := parseImeta(tag[1:])
			if att != nil {
				note.Attachment = append(note.Attachment, *att)
			}
		}
	}

	return note
}

// ToAnnounce converts a kind-1 quote post or kind-6 repost to an AP Announce.
// Returns nil if no quote/repost target is found.
func ToAnnounce(event *nostr.Event, tc *TransmuteContext) *Activity {
	quoteID := findQuoteID(event)
	if quoteID == "" {
		return nil
	}

	return &Activity{
		ID:        tc.objectURL(event.ID),
		Type:      "Announce",
		Actor:     tc.actorURL(event.PubKey),
		Object:    tc.objectURL(quoteID),
		Published: NostrDate(event.CreatedAt),
		To:        []string{PublicURI},
		CC:        []string{tc.actorURL(event.PubKey) + "/followers"},
		ProxyOf:   []Proxy{toNoteProxy(event)},
	}
}

// ToLike converts a kind-7 "+" reaction to an AP Like.
func ToLike(event *nostr.Event, tc *TransmuteContext) *Activity {
	reactedID := findLastEventTag(event)
	if reactedID == "" {
		return nil
	}

	act := &Activity{
		ID:      tc.objectURL(event.ID),
		Type:    "Like",
		Actor:   tc.actorURL(event.PubKey),
		Object:  tc.objectURL(reactedID),
		To:      []string{PublicURI},
		CC:      []string{tc.actorURL(event.PubKey) + "/followers"},
		ProxyOf: []Proxy{toNoteProxy(event)},
	}

	// Add p-tag targets.
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "p" {
			act.To = append(act.To, tc.actorURL(tag[1]))
		}
	}
	return act
}

// ToEmojiReact converts a kind-7 emoji reaction to an AP EmojiReact.
func ToEmojiReact(event *nostr.Event, tc *TransmuteContext) map[string]interface{} {
	reactedID := findLastEventTag(event)
	if reactedID == "" {
		return nil
	}

	obj := map[string]interface{}{
		"id":      tc.objectURL(event.ID),
		"type":    "EmojiReact",
		"actor":   tc.actorURL(event.PubKey),
		"object":  tc.objectURL(reactedID),
		"content": event.Content,
		"to":      []string{PublicURI},
		"cc":      []string{tc.actorURL(event.PubKey) + "/followers"},
		"proxyOf": []Proxy{toNoteProxy(event)},
	}

	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "p" {
			if to, ok := obj["to"].([]string); ok {
				obj["to"] = append(to, tc.actorURL(tag[1]))
			}
		}
	}
	return obj
}

// ToDelete converts a kind-5 deletion event to an AP Delete.
func ToDelete(event *nostr.Event, tc *TransmuteContext) *Activity {
	deletedID := findLastEventTag(event)
	if deletedID == "" {
		return nil
	}

	return &Activity{
		ID:      tc.objectURL(event.ID),
		Type:    "Delete",
		Actor:   tc.actorURL(event.PubKey),
		Object:  tc.objectURL(deletedID),
		To:      []string{PublicURI},
		CC:      []string{tc.actorURL(event.PubKey) + "/followers"},
		ProxyOf: []Proxy{toNoteProxy(event)},
	}
}

// BuildCreate wraps a Note in a Create activity.
func BuildCreate(note *Note, localDomain string) map[string]interface{} {
	return map[string]interface{}{
		"@context":  DefaultContext,
		"id":        note.ID + "/activity",
		"type":      "Create",
		"actor":     note.AttributedTo,
		"object":    note,
		"to":        note.To,
		"cc":        note.CC,
		"published": note.Published,
	}
}

// BuildUpdate wraps an Actor in an Update activity.
func BuildUpdate(actor *Actor) map[string]interface{} {
	return map[string]interface{}{
		"@context":  DefaultContext,
		"id":        actor.ID + "#update-" + fmt.Sprintf("%d", time.Now().Unix()),
		"type":      "Update",
		"actor":     actor.ID,
		"object":    actor,
		"to":        []string{PublicURI},
		"cc":        []string{actor.Followers},
		"published": time.Now().UTC().Format(time.RFC3339),
	}
}

// BuildFollow creates an AP Follow activity.
func BuildFollow(followerID, followedID string) map[string]interface{} {
	return map[string]interface{}{
		"@context": DefaultContext,
		"id":       followerID + "#follow-" + fmt.Sprintf("%d", time.Now().Unix()),
		"type":     "Follow",
		"actor":    followerID,
		"object":   followedID,
	}
}

// BuildAccept creates an AP Accept activity for a Follow.
func BuildAccept(followActivity map[string]interface{}, localActorID string) map[string]interface{} {
	return map[string]interface{}{
		"@context": DefaultContext,
		"id":       localActorID + "#accept-" + fmt.Sprintf("%d", time.Now().Unix()),
		"type":     "Accept",
		"actor":    localActorID,
		"object":   followActivity,
	}
}

// IsRepost returns true if a kind-1 event is a pure repost (no content, has quote tag).
func IsRepost(event *nostr.Event) bool {
	if event.Content != "" && !regexp.MustCompile(`^#\[\d+\]$`).MatchString(event.Content) {
		return false
	}
	return findQuoteID(event) != ""
}

// IsProxyEvent returns true if this event was created by the bridge (has a proxy tag).
func IsProxyEvent(event *nostr.Event) bool {
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "proxy" {
			return true
		}
	}
	return false
}

// ─── Internal helpers ────────────────────────────────────────────────────────

type nostrMeta struct {
	Name    string
	About   string
	Picture string
	Banner  string
	Fields  [][]string
}

func parseMetadata(content string) nostrMeta {
	// Simple JSON parse for common fields.
	// In production this would use a proper JSON parser.
	var meta nostrMeta
	// Use basic string extraction for robustness.
	meta.Name = extractJSONString(content, "name")
	meta.About = extractJSONString(content, "about")
	meta.Picture = extractJSONString(content, "picture")
	meta.Banner = extractJSONString(content, "banner")
	return meta
}

func extractJSONString(json, key string) string {
	// Very simple extractor — good enough for our use case.
	// A real implementation would use encoding/json.
	start := strings.Index(json, `"`+key+`"`)
	if start == -1 {
		return ""
	}
	rest := json[start+len(key)+2:]
	// Skip : and whitespace
	colon := strings.IndexByte(rest, ':')
	if colon == -1 {
		return ""
	}
	rest = strings.TrimSpace(rest[colon+1:])
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	var buf strings.Builder
	escaped := false
	for _, c := range rest {
		if escaped {
			switch c {
			case 'n':
				buf.WriteByte('\n')
			case 't':
				buf.WriteByte('\t')
			case '"':
				buf.WriteByte('"')
			case '\\':
				buf.WriteByte('\\')
			default:
				buf.WriteRune(c)
			}
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			break
		}
		buf.WriteRune(c)
	}
	return buf.String()
}

func linkifyText(text string) string {
	if text == "" {
		return ""
	}
	// Escape HTML entities.
	escaped := html.EscapeString(text)
	// Convert line breaks.
	escaped = strings.ReplaceAll(escaped, "\n", "<br />")
	// Linkify URLs.
	escaped = urlRe.ReplaceAllStringFunc(escaped, func(u string) string {
		return fmt.Sprintf(`<a href="%s" rel="nofollow noopener noreferrer" target="_blank">%s</a>`, u, u)
	})
	return escaped
}

func renderContent(content string, tags nostr.Tags, tc *TransmuteContext) string {
	if content == "" {
		return ""
	}

	// Remove trailing #[n] references.
	content = tagRefRe.ReplaceAllString(content, "")
	content = trailingRe.ReplaceAllString(content, "")

	// Replace inline nostr: URIs with AP links.
	content = mentionRe.ReplaceAllStringFunc(content, func(s string) string {
		// Just render as a link for now.
		return fmt.Sprintf(`<a href="%s/users/%s">%s</a>`, tc.LocalDomain, s[6:14], s[:14]+"...")
	})

	// Escape and linkify.
	return linkifyText(content)
}

func findReplyTag(event *nostr.Event) string {
	// Look for the "reply" marker first, then fall back to root.
	for _, tag := range event.Tags {
		if len(tag) >= 4 && tag[0] == "e" && tag[3] == "reply" {
			return tag[1]
		}
	}
	for _, tag := range event.Tags {
		if len(tag) >= 4 && tag[0] == "e" && tag[3] == "root" {
			return tag[1]
		}
	}
	// Plain e tag fallback.
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			return tag[1]
		}
	}
	return ""
}

func findQuoteID(event *nostr.Event) string {
	// NIP-18 q tag.
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "q" {
			return tag[1]
		}
	}
	// Legacy: last e tag when content is empty or just a #[n] reference.
	for i := len(event.Tags) - 1; i >= 0; i-- {
		tag := event.Tags[i]
		if len(tag) >= 2 && tag[0] == "e" {
			return tag[1]
		}
	}
	return ""
}

func findLastEventTag(event *nostr.Event) string {
	for i := len(event.Tags) - 1; i >= 0; i-- {
		tag := event.Tags[i]
		if len(tag) >= 2 && tag[0] == "e" {
			return tag[1]
		}
	}
	return ""
}

func parseImeta(entries []string) *Attachment {
	att := &Attachment{Type: "Document"}
	for _, entry := range entries {
		parts := strings.SplitN(entry, " ", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "url":
			att.URL = parts[1]
		case "m":
			att.MediaType = parts[1]
		case "blurhash":
			att.Blurhash = parts[1]
		case "dim":
			fmt.Sscanf(parts[1], "%dx%d", &att.Width, &att.Height)
		}
	}
	if att.URL == "" {
		return nil
	}
	return att
}

func toNpubProxy(event *nostr.Event) Proxy {
	return Proxy{
		Protocol:      NostrProtocolURI,
		Proxied:       "npub1" + event.PubKey[:8], // simplified; would use nip19 encoding
		Authoritative: true,
	}
}

func toNoteProxy(event *nostr.Event) Proxy {
	return Proxy{
		Protocol:      NostrProtocolURI,
		Proxied:       "note1" + event.ID[:8], // simplified
		Authoritative: true,
	}
}
