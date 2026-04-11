package bsky

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

// ─── NostrNoteToFeedPost ─────────────────────────────────────────────────────

func TestNostrNoteToFeedPost(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantText    string
		wantFacets  int
		wantTrunc   bool // expect truncation suffix
	}{
		{
			name:       "simple text post",
			content:    "Hello world",
			wantText:   "Hello world",
			wantFacets: 0,
		},
		{
			name:       "post with URL",
			content:    "Check out https://example.com/page",
			wantText:   "Check out https://example.com/page",
			wantFacets: 1,
		},
		{
			name:       "post with hashtag",
			content:    "Hello #nostr",
			wantText:   "Hello #nostr",
			wantFacets: 1,
		},
		{
			name:       "empty content",
			content:    "",
			wantText:   "",
			wantFacets: 0,
		},
		{
			name:      "post exceeding 300 graphemes is truncated",
			content:   strings.Repeat("a", 350),
			wantTrunc: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &nostr.Event{
				ID:        "abc123",
				PubKey:    "pubkey123",
				Content:   tt.content,
				CreatedAt: nostr.Timestamp(1700000000),
				Tags:      nostr.Tags{},
			}

			post, err := NostrNoteToFeedPost(event, "https://njump.me", nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantTrunc {
				if !strings.Contains(post.Text, "…") {
					t.Errorf("expected truncation suffix '…' in text, got: %s", post.Text)
				}
				if graphemeCount(post.Text) > maxGraphemes {
					t.Errorf("truncated text exceeds %d graphemes: got %d", maxGraphemes, graphemeCount(post.Text))
				}
				return
			}

			if post.Text != tt.wantText {
				t.Errorf("text = %q, want %q", post.Text, tt.wantText)
			}
			if len(post.Facets) != tt.wantFacets {
				t.Errorf("facets count = %d, want %d", len(post.Facets), tt.wantFacets)
			}
			if post.Type != feedPostType {
				t.Errorf("type = %q, want %q", post.Type, feedPostType)
			}
		})
	}
}

// ─── buildFacets ─────────────────────────────────────────────────────────────

func TestBuildFacets(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantCount  int
		wantTypes  []string // expected facet feature types in order
	}{
		{
			name:      "no facets in plain text",
			text:      "Hello world",
			wantCount: 0,
		},
		{
			name:      "single URL",
			text:      "Visit https://example.com today",
			wantCount: 1,
			wantTypes: []string{facetLinkType},
		},
		{
			name:      "single hashtag",
			text:      "Hello #nostr",
			wantCount: 1,
			wantTypes: []string{facetTagType},
		},
		{
			name:      "URL and hashtag",
			text:      "Check https://example.com #golang",
			wantCount: 2,
			wantTypes: []string{facetLinkType, facetTagType},
		},
		{
			name:      "multiple URLs",
			text:      "Links: https://a.com and https://b.com",
			wantCount: 2,
			wantTypes: []string{facetLinkType, facetLinkType},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facets := buildFacets(tt.text, nil)
			if len(facets) != tt.wantCount {
				t.Fatalf("facet count = %d, want %d", len(facets), tt.wantCount)
			}
			for i, f := range facets {
				if len(f.Features) == 0 {
					t.Fatalf("facet %d has no features", i)
				}
				if i < len(tt.wantTypes) && f.Features[0].Type != tt.wantTypes[i] {
					t.Errorf("facet %d type = %q, want %q", i, f.Features[0].Type, tt.wantTypes[i])
				}
				// Verify byte offsets are within bounds.
				if f.Index.ByteStart < 0 || f.Index.ByteEnd > len(tt.text) {
					t.Errorf("facet %d byte offsets out of range: [%d, %d) in text of len %d",
						i, f.Index.ByteStart, f.Index.ByteEnd, len(tt.text))
				}
			}
		})
	}
}

func TestBuildFacets_URLByteOffsets(t *testing.T) {
	text := "Go to https://example.com now"
	facets := buildFacets(text, nil)
	if len(facets) != 1 {
		t.Fatalf("expected 1 facet, got %d", len(facets))
	}
	extracted := text[facets[0].Index.ByteStart:facets[0].Index.ByteEnd]
	if extracted != "https://example.com" {
		t.Errorf("extracted URL = %q, want %q", extracted, "https://example.com")
	}
	if facets[0].Features[0].URI != "https://example.com" {
		t.Errorf("facet URI = %q, want %q", facets[0].Features[0].URI, "https://example.com")
	}
}

func TestBuildFacets_HashtagByteOffsets(t *testing.T) {
	text := "Hello #nostr world"
	facets := buildFacets(text, nil)
	if len(facets) != 1 {
		t.Fatalf("expected 1 facet, got %d", len(facets))
	}
	extracted := text[facets[0].Index.ByteStart:facets[0].Index.ByteEnd]
	if extracted != "#nostr" {
		t.Errorf("extracted tag = %q, want %q", extracted, "#nostr")
	}
	if facets[0].Features[0].Tag != "nostr" {
		t.Errorf("facet tag = %q, want %q", facets[0].Features[0].Tag, "nostr")
	}
}

// ─── extractContentFromRecord ────────────────────────────────────────────────

func TestExtractContentFromRecord(t *testing.T) {
	tests := []struct {
		name   string
		record map[string]interface{}
		want   string
	}{
		{
			name:   "plain text record",
			record: map[string]interface{}{"text": "Hello world"},
			want:   "Hello world",
		},
		{
			name: "record with hidden facet link",
			record: map[string]interface{}{
				"text": "Click here",
				"facets": []interface{}{
					map[string]interface{}{
						"features": []interface{}{
							map[string]interface{}{
								"$type": facetLinkType,
								"uri":   "https://example.com/hidden",
							},
						},
					},
				},
			},
			want: "Click here\n\nhttps://example.com/hidden",
		},
		{
			name: "record with external embed link",
			record: map[string]interface{}{
				"text": "Check this out",
				"embed": map[string]interface{}{
					"$type": "app.bsky.embed.external",
					"external": map[string]interface{}{
						"uri": "https://example.com/card",
					},
				},
			},
			want: "Check this out\n\nhttps://example.com/card",
		},
		{
			name:   "empty text",
			record: map[string]interface{}{"text": ""},
			want:   "",
		},
		{
			name:   "nil record",
			record: nil,
			want:   "",
		},
		{
			name: "link already in text is not duplicated",
			record: map[string]interface{}{
				"text": "Visit https://example.com",
				"facets": []interface{}{
					map[string]interface{}{
						"features": []interface{}{
							map[string]interface{}{
								"$type": facetLinkType,
								"uri":   "https://example.com",
							},
						},
					},
				},
			},
			want: "Visit https://example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContentFromRecord(tt.record)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ─── ExtractImetaTags ────────────────────────────────────────────────────────

func TestExtractImetaTags(t *testing.T) {
	tests := []struct {
		name      string
		tags      nostr.Tags
		maxImages int
		wantCount int
		wantURL   string
		wantAlt   string
		wantMime  string
		wantW     int
		wantH     int
	}{
		{
			name: "event with imeta tags",
			tags: nostr.Tags{
				{"imeta", "url https://cdn.example.com/img.jpg", "alt A photo", "m image/jpeg", "dim 1920x1080"},
			},
			maxImages: 4,
			wantCount: 1,
			wantURL:   "https://cdn.example.com/img.jpg",
			wantAlt:   "A photo",
			wantMime:  "image/jpeg",
			wantW:     1920,
			wantH:     1080,
		},
		{
			name:      "event with no imeta tags",
			tags:      nostr.Tags{{"t", "nostr"}},
			maxImages: 4,
			wantCount: 0,
		},
		{
			name: "max images limit respected",
			tags: nostr.Tags{
				{"imeta", "url https://cdn.example.com/1.jpg"},
				{"imeta", "url https://cdn.example.com/2.jpg"},
				{"imeta", "url https://cdn.example.com/3.jpg"},
			},
			maxImages: 2,
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &nostr.Event{Tags: tt.tags}
			result := ExtractImetaTags(event, tt.maxImages)
			if len(result) != tt.wantCount {
				t.Fatalf("count = %d, want %d", len(result), tt.wantCount)
			}
			if tt.wantCount > 0 && tt.wantURL != "" {
				if result[0].URL != tt.wantURL {
					t.Errorf("URL = %q, want %q", result[0].URL, tt.wantURL)
				}
				if result[0].Alt != tt.wantAlt {
					t.Errorf("Alt = %q, want %q", result[0].Alt, tt.wantAlt)
				}
				if result[0].MimeType != tt.wantMime {
					t.Errorf("MimeType = %q, want %q", result[0].MimeType, tt.wantMime)
				}
				if result[0].Width != tt.wantW {
					t.Errorf("Width = %d, want %d", result[0].Width, tt.wantW)
				}
				if result[0].Height != tt.wantH {
					t.Errorf("Height = %d, want %d", result[0].Height, tt.wantH)
				}
			}
		})
	}
}

// ─── BuildSelfLabels ─────────────────────────────────────────────────────────

func TestBuildSelfLabels(t *testing.T) {
	tests := []struct {
		name    string
		tags    nostr.Tags
		wantNil bool
		wantVal string
	}{
		{
			name:    "event with content-warning tag",
			tags:    nostr.Tags{{"content-warning", "NSFW"}},
			wantNil: false,
			wantVal: "!warn",
		},
		{
			name:    "event without content-warning",
			tags:    nostr.Tags{{"t", "nostr"}},
			wantNil: true,
		},
		{
			name:    "content-warning tag with no value",
			tags:    nostr.Tags{{"content-warning"}},
			wantNil: false,
			wantVal: "!warn",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &nostr.Event{Tags: tt.tags}
			got := BuildSelfLabels(event)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil SelfLabels")
			}
			if got.Type != "com.atproto.label.defs#selfLabels" {
				t.Errorf("type = %q, want %q", got.Type, "com.atproto.label.defs#selfLabels")
			}
			if len(got.Values) != 1 || got.Values[0].Val != tt.wantVal {
				t.Errorf("values = %+v, want [{Val: %q}]", got.Values, tt.wantVal)
			}
		})
	}
}
