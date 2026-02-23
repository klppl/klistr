// Package bsky implements the Bluesky (AT Protocol) bridge for klistr.
// It provides outbound mirroring of Nostr events to Bluesky and inbound
// polling of Bluesky notifications to Nostr events.
package bsky

// ─── Auth ─────────────────────────────────────────────────────────────────────

// Session holds credentials returned by com.atproto.server.createSession.
type Session struct {
	DID        string `json:"did"`
	Handle     string `json:"handle"`
	AccessJwt  string `json:"accessJwt"`
	RefreshJwt string `json:"refreshJwt"`
}

// CreateSessionInput is the request body for com.atproto.server.createSession.
type CreateSessionInput struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

// ─── Record operations ────────────────────────────────────────────────────────

// CreateRecordRequest is the request body for com.atproto.repo.createRecord.
type CreateRecordRequest struct {
	Repo       string      `json:"repo"`
	Collection string      `json:"collection"`
	Record     interface{} `json:"record"`
}

// CreateRecordResponse is returned by com.atproto.repo.createRecord.
type CreateRecordResponse struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// DeleteRecordRequest is the request body for com.atproto.repo.deleteRecord.
type DeleteRecordRequest struct {
	Repo       string `json:"repo"`
	Collection string `json:"collection"`
	RKey       string `json:"rkey"`
}

// ─── Feed post record (app.bsky.feed.post) ────────────────────────────────────

// FeedPost is the lexicon record for a Bluesky post.
type FeedPost struct {
	Type      string   `json:"$type"`
	Text      string   `json:"text"`
	CreatedAt string   `json:"createdAt"`
	Facets    []Facet  `json:"facets,omitempty"`
	Reply     *Reply   `json:"reply,omitempty"`
	Langs     []string `json:"langs,omitempty"`
}

// Facet describes rich-text annotations (links, mentions, tags).
type Facet struct {
	Index    ByteSlice      `json:"index"`
	Features []FacetFeature `json:"features"`
}

// ByteSlice marks the byte range of a facet in the post text.
type ByteSlice struct {
	ByteStart int `json:"byteStart"`
	ByteEnd   int `json:"byteEnd"`
}

// FacetFeature is one annotation within a facet. The $type field selects the variant:
//   - app.bsky.richtext.facet#link  → URI field is set
//   - app.bsky.richtext.facet#mention → DID field is set
//   - app.bsky.richtext.facet#tag   → Tag field is set
type FacetFeature struct {
	Type string `json:"$type"`
	URI  string `json:"uri,omitempty"`
	DID  string `json:"did,omitempty"`
	Tag  string `json:"tag,omitempty"`
}

// Reply holds root/parent references for a threaded reply.
type Reply struct {
	Root   Ref `json:"root"`
	Parent Ref `json:"parent"`
}

// Ref is a CID+URI pair identifying an AT Protocol record.
type Ref struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// ─── Like / Repost records ────────────────────────────────────────────────────

// LikeRecord is the lexicon record for app.bsky.feed.like.
type LikeRecord struct {
	Type      string `json:"$type"`
	Subject   Ref    `json:"subject"`
	CreatedAt string `json:"createdAt"`
}

// RepostRecord is the lexicon record for app.bsky.feed.repost.
type RepostRecord struct {
	Type      string `json:"$type"`
	Subject   Ref    `json:"subject"`
	CreatedAt string `json:"createdAt"`
}

// ─── Notifications ────────────────────────────────────────────────────────────

// Notification is a single entry from app.bsky.notification.listNotifications.
// Reason values: "like" | "repost" | "follow" | "mention" | "reply" | "quote"
type Notification struct {
	URI       string      `json:"uri"`
	CID       string      `json:"cid"`
	Author    NotifAuthor `json:"author"`
	Reason    string      `json:"reason"`
	Record    interface{} `json:"record"`
	IsRead    bool        `json:"isRead"`
	IndexedAt string      `json:"indexedAt"`
}

// NotifAuthor holds basic author info for a notification.
type NotifAuthor struct {
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
}

// ListNotificationsResponse is returned by app.bsky.notification.listNotifications.
type ListNotificationsResponse struct {
	Notifications []Notification `json:"notifications"`
	Cursor        string         `json:"cursor"`
}

// ─── Timeline feed (app.bsky.feed.getTimeline) ────────────────────────────────

// TimelineFeedPost is one entry in the timeline feed response.
// Reason is non-nil when the post appears because someone the user follows
// reposted it (app.bsky.feed.defs#reasonRepost).
type TimelineFeedPost struct {
	Post   TimelinePost `json:"post"`
	Reason *FeedReason  `json:"reason,omitempty"`
}

// TimelinePost holds the core post data within a timeline feed item.
type TimelinePost struct {
	URI       string      `json:"uri"`
	CID       string      `json:"cid"`
	Author    NotifAuthor `json:"author"`
	Record    interface{} `json:"record"`
	IndexedAt string      `json:"indexedAt"`
}

// FeedReason indicates why a post appears in the timeline.
// The only current variant is app.bsky.feed.defs#reasonRepost.
type FeedReason struct {
	Type string      `json:"$type"`
	By   NotifAuthor `json:"by"`
}

// GetTimelineResponse is returned by app.bsky.feed.getTimeline.
type GetTimelineResponse struct {
	Feed   []TimelineFeedPost `json:"feed"`
	Cursor string             `json:"cursor"`
}

// ─── Profile ──────────────────────────────────────────────────────────────────

// Profile is returned by app.bsky.actor.getProfile.
type Profile struct {
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Avatar      string `json:"avatar"`
	Banner      string `json:"banner"`
}
