package ap

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
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
	tagRefRe   = regexp.MustCompile(`\n{0,2}#\[\d+\]$`)
	trailingRe = regexp.MustCompile(`\s+$`)
	urlRe      = regexp.MustCompile(`https?://[^\s<>"{}|\\^` + "`" + `\[\]]+`)
	mentionRe  = regexp.MustCompile(`nostr:(npub|nprofile|note|nevent|naddr)[a-z0-9]+`)

	// Markdown inline patterns used by markdownToHTML.
	mdBoldRe   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdItalicRe = regexp.MustCompile(`\*([^*\n]+?)\*`)
	mdCodeRe   = regexp.MustCompile("`([^`\n]+)`")
	mdLinkRe   = regexp.MustCompile(`\[([^\[\]]+)\]\((https?://[^)\s]+)\)`)
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
		case len(tag) >= 2 && tag[0] == "expiration":
			// NIP-40: map expiration unix timestamp to AP endTime (RFC3339).
			var ts int64
			if _, err := fmt.Sscanf(tag[1], "%d", &ts); err == nil && ts > 0 {
				note.EndTime = time.Unix(ts, 0).UTC().Format(time.RFC3339)
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

// ToZap converts a kind-9735 zap receipt to an AP Zap activity.
// The Zap type is present in DefaultContext via the mostr.pub namespace.
// AP servers that do not recognise the type will silently discard the activity.
func ToZap(event *nostr.Event, tc *TransmuteContext) map[string]interface{} {
	// Zap receipts target a specific note via 'e' tag; skip profile-only zaps.
	var reactedID string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			reactedID = tag[1]
			break
		}
	}
	if reactedID == "" {
		return nil
	}

	// Parse the embedded zap request (kind-9734) from the description tag to
	// extract the sats amount and optional comment left by the zap sender.
	var amountMsats int64
	var comment string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "description" {
			var zapReq struct {
				Content string     `json:"content"`
				Tags    [][]string `json:"tags"`
			}
			if err := json.Unmarshal([]byte(tag[1]), &zapReq); err == nil {
				comment = zapReq.Content
				for _, t := range zapReq.Tags {
					if len(t) >= 2 && t[0] == "amount" {
						fmt.Sscanf(t[1], "%d", &amountMsats)
					}
				}
			}
			break
		}
	}

	// Build human-readable content (sats + optional comment).
	content := comment
	if amountMsats > 0 {
		sats := amountMsats / 1000
		if comment != "" {
			content = fmt.Sprintf("⚡ %d sats: %s", sats, comment)
		} else {
			content = fmt.Sprintf("⚡ %d sats", sats)
		}
	}

	act := map[string]interface{}{
		"@context": DefaultContext,
		"id":       tc.objectURL(event.ID),
		"type":     "Zap",
		"actor":    tc.LocalActorURL,
		"object":   tc.objectURL(reactedID),
		"to":       []string{PublicURI},
		"cc":       []string{tc.LocalActorURL + "/followers"},
		"proxyOf":  []Proxy{toNoteProxy(event)},
	}
	if content != "" {
		act["content"] = content
	}
	return act
}

// ToQuestion converts a Nostr kind-1068 poll event (NIP-69) to an AP Question object.
// Returns nil if the event has no poll options.
func ToQuestion(event *nostr.Event, tc *TransmuteContext) *Note {
	var pollType, closedAt string

	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "poll_type":
			pollType = tag[1]
		case "closed_at":
			var t int64
			if _, err := fmt.Sscanf(tag[1], "%d", &t); err == nil && t > 0 {
				closedAt = time.Unix(t, 0).UTC().Format(time.RFC3339)
			}
		}
	}

	// Collect options in NIP-69 order (poll_option index tag[1], text tag[2]).
	var opts []QuestionOption
	for _, tag := range event.Tags {
		if len(tag) >= 3 && tag[0] == "poll_option" {
			opts = append(opts, QuestionOption{Type: "Note", Name: tag[2]})
		}
	}
	if len(opts) == 0 {
		return nil
	}

	q := &Note{
		ID:           tc.objectURL(event.ID),
		Type:         "Question",
		AttributedTo: tc.LocalActorURL,
		Content:      html.EscapeString(event.Content),
		Published:    event.CreatedAt.Time().UTC().Format(time.RFC3339),
		To:           []string{PublicURI},
		CC:           []string{tc.LocalActorURL + "/followers"},
		EndTime:      closedAt,
		ProxyOf:      []Proxy{toNoteProxy(event)},
	}

	if pollType == "multiplechoice" {
		q.AnyOf = opts
	} else {
		q.OneOf = opts
	}

	return q
}

// ToArticle converts a Nostr kind-30023 long-form article to an AP Article object.
// The returned *Note has Type="Article" and Name set to the article title.
func ToArticle(event *nostr.Event, tc *TransmuteContext) *Note {
	var title, summary, imageURL, publishedAt string
	var hashtags []interface{}

	var endTime string
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "title":
			title = tag[1]
		case "summary":
			summary = tag[1]
		case "image":
			imageURL = tag[1]
		case "published_at":
			publishedAt = tag[1]
		case "t":
			hashtags = append(hashtags, Hashtag{
				Type: "Hashtag",
				Href: tc.baseURL("/tags/" + tag[1]),
				Name: "#" + tag[1],
			})
		case "expiration":
			var ts int64
			if _, err := fmt.Sscanf(tag[1], "%d", &ts); err == nil && ts > 0 {
				endTime = time.Unix(ts, 0).UTC().Format(time.RFC3339)
			}
		}
	}

	// Prefer published_at for the canonical timestamp.
	ts := event.CreatedAt
	if publishedAt != "" {
		var t int64
		if _, err := fmt.Sscanf(publishedAt, "%d", &t); err == nil && t > 0 {
			ts = nostr.Timestamp(t)
		}
	}

	articleURL := tc.objectURL(event.ID)

	note := &Note{
		ID:           articleURL,
		Type:         "Article",
		Name:         title,
		AttributedTo: tc.actorURL(event.PubKey),
		Content:      markdownToHTML(event.Content),
		Summary:      summary,
		Published:    NostrDate(ts),
		URL:          articleURL,
		To:           []string{PublicURI},
		CC:           []string{tc.actorURL(event.PubKey) + "/followers"},
		Tag:          hashtags,
		EndTime:      endTime,
		Generator: &Generator{
			Type: "Application",
			Name: "klistr",
			URL:  "https://github.com/klppl/klistr",
		},
		ProxyOf: []Proxy{toNoteProxy(event)},
	}

	if imageURL != "" {
		note.Attachment = append(note.Attachment, Attachment{
			Type: "Image",
			URL:  imageURL,
		})
	}

	return note
}

// markdownToHTML converts a Markdown string to HTML. It handles the most common
// constructs: fenced code blocks, headings (h1–h3), paragraphs, and inline
// elements (bold, italic, inline code, links). Edge cases like nested emphasis
// or definition lists are not handled but the plain text is still preserved.
func markdownToHTML(md string) string {
	if md == "" {
		return ""
	}
	var out strings.Builder
	// Split on fenced code block markers; odd-indexed segments are code content.
	segments := strings.Split(md, "```")
	for i, seg := range segments {
		if i%2 == 0 {
			out.WriteString(renderMarkdownBlocks(seg))
		} else {
			// First line of seg is the optional language hint; skip it.
			nl := strings.IndexByte(seg, '\n')
			code := seg
			if nl >= 0 {
				code = seg[nl+1:]
			}
			out.WriteString("<pre><code>")
			out.WriteString(html.EscapeString(strings.TrimRight(code, "\n")))
			out.WriteString("</code></pre>")
		}
	}
	return out.String()
}

// renderMarkdownBlocks processes a non-code Markdown segment into HTML block elements.
func renderMarkdownBlocks(text string) string {
	var out strings.Builder
	for _, block := range strings.Split(text, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		switch {
		case block == "---" || block == "***" || block == "___":
			out.WriteString("<hr />")
		case strings.HasPrefix(block, "### "):
			out.WriteString("<h3>" + renderInlineMarkdown(block[4:]) + "</h3>")
		case strings.HasPrefix(block, "## "):
			out.WriteString("<h2>" + renderInlineMarkdown(block[3:]) + "</h2>")
		case strings.HasPrefix(block, "# "):
			out.WriteString("<h1>" + renderInlineMarkdown(block[2:]) + "</h1>")
		default:
			lines := strings.Split(block, "\n")
			rendered := make([]string, len(lines))
			for i, line := range lines {
				rendered[i] = renderInlineMarkdown(line)
			}
			out.WriteString("<p>" + strings.Join(rendered, "<br />") + "</p>")
		}
	}
	return out.String()
}

// renderInlineMarkdown converts inline Markdown syntax to HTML within a single line.
func renderInlineMarkdown(s string) string {
	s = html.EscapeString(s)
	// Links: [text](url)
	s = mdLinkRe.ReplaceAllString(s, `<a href="$2" rel="nofollow noopener noreferrer" target="_blank">$1</a>`)
	// Inline code: `code` (before bold/italic to protect content from further replacement)
	s = mdCodeRe.ReplaceAllString(s, "<code>$1</code>")
	// Bold: **text**
	s = mdBoldRe.ReplaceAllString(s, "<strong>$1</strong>")
	// Italic: *text* (remaining single asterisks after bold replacement)
	s = mdItalicRe.ReplaceAllString(s, "<em>$1</em>")
	return s
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
		"to":       []string{followedID},
	}
}

// BuildUndoFollow creates an AP Undo(Follow) activity.
func BuildUndoFollow(followerID, followedID string) map[string]interface{} {
	return map[string]interface{}{
		"@context": DefaultContext,
		"id":       followerID + "#unfollow-" + fmt.Sprintf("%d", time.Now().Unix()),
		"type":     "Undo",
		"actor":    followerID,
		"object":   BuildFollow(followerID, followedID),
		"to":       []string{followedID},
	}
}

// BuildAccept creates an AP Accept activity for a Follow.
func BuildAccept(followActivity map[string]interface{}, localActorID string, followerID string) map[string]interface{} {
	return map[string]interface{}{
		"@context": DefaultContext,
		"id":       localActorID + "#accept-" + fmt.Sprintf("%d", time.Now().Unix()),
		"type":     "Accept",
		"actor":    localActorID,
		"object":   followActivity,
		"to":       []string{followerID},
	}
}

// BuildReject creates an AP Reject activity for a Follow.
func BuildReject(followActivity map[string]interface{}, localActorID string, followerID string) map[string]interface{} {
	return map[string]interface{}{
		"@context": DefaultContext,
		"id":       localActorID + "#reject-" + fmt.Sprintf("%d", time.Now().Unix()),
		"type":     "Reject",
		"actor":    localActorID,
		"object":   followActivity,
		"to":       []string{followerID},
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
	Name    string     `json:"name"`
	About   string     `json:"about"`
	Picture string     `json:"picture"`
	Banner  string     `json:"banner"`
	Fields  [][]string `json:"fields"`
}

func parseMetadata(content string) nostrMeta {
	var meta nostrMeta
	_ = json.Unmarshal([]byte(content), &meta)
	return meta
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

	// Replace inline nostr: URIs with links to njump.me so AP readers can
	// follow them. This covers all NIP-19 types: npub, nprofile, note, nevent, naddr.
	content = mentionRe.ReplaceAllStringFunc(content, func(s string) string {
		bech32 := s[6:] // strip "nostr:" prefix
		label := bech32
		if len(label) > 16 {
			label = label[:16] + "…"
		}
		return fmt.Sprintf(`<a href="https://njump.me/%s">%s</a>`, bech32, label)
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
		case "alt":
			// AP uses "name" as the alt-text field for media attachments.
			att.Name = parts[1]
		case "fallback":
			// Store fallback as a second-priority URL. If the primary URL is
			// empty (shouldn't happen) promote fallback to URL.
			if att.URL == "" {
				att.URL = parts[1]
			}
			// Otherwise we keep the fallback for informational purposes;
			// AP does not have a standard fallback field so it is silently
			// available to callers that inspect the raw map.
		}
	}
	if att.URL == "" {
		return nil
	}
	// Set the AP attachment type from the MIME prefix.
	switch {
	case strings.HasPrefix(att.MediaType, "image/"):
		att.Type = "Image"
	case strings.HasPrefix(att.MediaType, "video/"):
		att.Type = "Video"
	case strings.HasPrefix(att.MediaType, "audio/"):
		att.Type = "Audio"
	}
	return att
}

func toNpubProxy(event *nostr.Event) Proxy {
	npub, err := nip19.EncodePublicKey(event.PubKey)
	if err != nil {
		npub = event.PubKey
	}
	return Proxy{
		Protocol:      NostrProtocolURI,
		Proxied:       npub,
		Authoritative: true,
	}
}

func toNoteProxy(event *nostr.Event) Proxy {
	note, err := nip19.EncodeNote(event.ID)
	if err != nil {
		note = event.ID
	}
	return Proxy{
		Protocol:      NostrProtocolURI,
		Proxied:       note,
		Authoritative: true,
	}
}
