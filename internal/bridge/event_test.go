package bridge

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

// findTag returns the first tag whose first element equals name, or nil.
func findTags(tags nostr.Tags, name string) []nostr.Tag {
	var out []nostr.Tag
	for _, t := range tags {
		if len(t) >= 1 && t[0] == name {
			out = append(out, t)
		}
	}
	return out
}

func TestBuildKind1Event_DirectReplyToRoot(t *testing.T) {
	// A direct reply to the thread root must have a single "e" tag marked "root"
	// (NIP-10), plus a "p" tag for the replied-to author.
	ev := BuildKind1Event(NormalizedPost{
		Content:        "a reply",
		ReplyToEventID: "rootid",
		RootEventID:    "rootid",
		ReplyToPubkey:  "authorpk",
		RootPubkey:     "authorpk",
		RelayHint:      "wss://relay.example",
	})

	eTags := findTags(ev.Tags, "e")
	if len(eTags) != 1 {
		t.Fatalf("want 1 e-tag, got %d: %v", len(eTags), eTags)
	}
	if eTags[0][1] != "rootid" || eTags[0][3] != "root" {
		t.Errorf("e-tag = %v, want id=rootid marker=root", eTags[0])
	}

	pTags := findTags(ev.Tags, "p")
	if len(pTags) != 1 || pTags[0][1] != "authorpk" {
		t.Errorf("p-tags = %v, want single authorpk", pTags)
	}
}

func TestBuildKind1Event_NestedReply(t *testing.T) {
	// A reply to a non-root post emits root + reply markers and p-tags for both
	// the parent and root authors (deduped).
	ev := BuildKind1Event(NormalizedPost{
		Content:        "deep reply",
		ReplyToEventID: "parentid",
		RootEventID:    "rootid",
		ReplyToPubkey:  "parentpk",
		RootPubkey:     "rootpk",
	})

	eTags := findTags(ev.Tags, "e")
	if len(eTags) != 2 {
		t.Fatalf("want 2 e-tags, got %d: %v", len(eTags), eTags)
	}
	if eTags[0][1] != "rootid" || eTags[0][3] != "root" {
		t.Errorf("first e-tag = %v, want rootid/root", eTags[0])
	}
	if eTags[1][1] != "parentid" || eTags[1][3] != "reply" {
		t.Errorf("second e-tag = %v, want parentid/reply", eTags[1])
	}

	pTags := findTags(ev.Tags, "p")
	if len(pTags) != 2 {
		t.Fatalf("want 2 p-tags, got %d: %v", len(pTags), pTags)
	}
}

func TestBuildKind1Event_PTagDedup(t *testing.T) {
	// Parent author that is also an explicit mention must not be duplicated.
	ev := BuildKind1Event(NormalizedPost{
		Content:        "hi",
		ReplyToEventID: "rootid",
		RootEventID:    "rootid",
		ReplyToPubkey:  "samepk",
		RootPubkey:     "samepk",
		MentionPubkeys: []string{"samepk", "otherpk"},
	})

	pTags := findTags(ev.Tags, "p")
	if len(pTags) != 2 {
		t.Fatalf("want 2 deduped p-tags, got %d: %v", len(pTags), pTags)
	}
	seen := map[string]bool{}
	for _, p := range pTags {
		if seen[p[1]] {
			t.Errorf("duplicate p-tag for %s", p[1])
		}
		seen[p[1]] = true
	}
}

func TestBuildKind1Event_NonReplyHasNoThreadTags(t *testing.T) {
	ev := BuildKind1Event(NormalizedPost{Content: "top level post"})
	if len(findTags(ev.Tags, "e")) != 0 {
		t.Errorf("top-level post should have no e-tags: %v", ev.Tags)
	}
	if len(findTags(ev.Tags, "p")) != 0 {
		t.Errorf("top-level post should have no p-tags: %v", ev.Tags)
	}
}
