// Package ap implements the ActivityPub protocol for the klistr bridge.
package ap

import (
	"encoding/json"
	"fmt"
)

// StringOrArray deserialises an AP field that may be either a JSON string
// or a JSON array of strings (both are valid per the AP spec).
type StringOrArray []string

func (s *StringOrArray) UnmarshalJSON(data []byte) error {
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*s = arr
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = []string{str}
		return nil
	}
	return fmt.Errorf("cannot unmarshal %s into string or []string", data)
}

const (
	PublicURI         = "https://www.w3.org/ns/activitystreams#Public"
	ActivityStreamsNS = "https://www.w3.org/ns/activitystreams"
	SecurityNS        = "https://w3id.org/security/v1"
	NostrProtocolURI  = "https://github.com/nostr-protocol/nostr"
)

// Context is the standard JSON-LD @context for ActivityPub objects.
var DefaultContext = []interface{}{
	ActivityStreamsNS,
	SecurityNS,
	map[string]interface{}{
		"Hashtag":       "as:Hashtag",
		"sensitive":     "as:sensitive",
		"schema":        "http://schema.org#",
		"PropertyValue": "schema:PropertyValue",
		"value":         "schema:value",
		"EmojiReact":    "http://joinmastodon.org/ns#EmojiReact",
		"Emoji":         "http://joinmastodon.org/ns#Emoji",
		"Zap":           "https://mostr.pub/ns#Zap",
		"proxyOf":       "https://mostr.pub/ns#proxyOf",
		"proxied":       "https://mostr.pub/ns#proxied",
		"protocol":      "https://mostr.pub/ns#protocol",
		"authoritative": "https://mostr.pub/ns#authoritative",
		"quoteUrl":      "as:quoteUrl",
	},
}

// Actor represents an ActivityPub actor (Person, Service, etc.).
type Actor struct {
	Context           interface{}     `json:"@context,omitempty"`
	ID                string          `json:"id"`
	Type              string          `json:"type"`
	Name              string          `json:"name,omitempty"`
	PreferredUsername string          `json:"preferredUsername"`
	Summary           string          `json:"summary,omitempty"`
	Inbox             string          `json:"inbox"`
	Outbox            string          `json:"outbox,omitempty"`
	Followers         string          `json:"followers,omitempty"`
	Following         string          `json:"following,omitempty"`
	PublicKey         *PublicKey      `json:"publicKey,omitempty"`
	Icon              *Image          `json:"icon,omitempty"`
	Image             *Image          `json:"image,omitempty"`
	Attachment        []PropertyValue `json:"attachment,omitempty"`
	Tag               []interface{}   `json:"tag,omitempty"`
	URL               string          `json:"url,omitempty"`
	Endpoints         *Endpoints      `json:"endpoints,omitempty"`
	ProxyOf           []Proxy         `json:"proxyOf,omitempty"`
}

// PublicKey represents an RSA public key attached to an actor.
type PublicKey struct {
	ID           string `json:"id"`
	Owner        string `json:"owner"`
	PublicKeyPem string `json:"publicKeyPem"`
}

// Image represents an ActivityPub Image object.
type Image struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// Endpoints holds shared inbox and other endpoints.
type Endpoints struct {
	SharedInbox string `json:"sharedInbox,omitempty"`
}

// PropertyValue is a key-value pair, used for profile fields.
type PropertyValue struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Note represents an ActivityPub Note (and related types: Article, Question).
type Note struct {
	Context      interface{}   `json:"@context,omitempty"`
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	AttributedTo string        `json:"attributedTo"`
	Name         string        `json:"name,omitempty"` // Article title or Question text
	Content      string        `json:"content"`
	Published    string        `json:"published,omitempty"`
	To           []string      `json:"to,omitempty"`
	CC           []string      `json:"cc,omitempty"`
	Tag          []interface{} `json:"tag,omitempty"`
	Attachment   []Attachment  `json:"attachment,omitempty"`
	URL          string        `json:"url,omitempty"`
	InReplyTo    string        `json:"inReplyTo,omitempty"`
	QuoteURL     string        `json:"quoteUrl,omitempty"`
	Sensitive    bool          `json:"sensitive,omitempty"`
	Summary      string        `json:"summary,omitempty"`
	Generator    *Generator    `json:"generator,omitempty"`
	ProxyOf      []Proxy       `json:"proxyOf,omitempty"`
	// Poll fields (type=Question only).
	OneOf       []QuestionOption `json:"oneOf,omitempty"`
	AnyOf       []QuestionOption `json:"anyOf,omitempty"`
	EndTime     string           `json:"endTime,omitempty"`
	Closed      string           `json:"closed,omitempty"`
	VotersCount int              `json:"votersCount,omitempty"`
}

// QuestionOption represents a single poll choice in an AP Question object.
type QuestionOption struct {
	Type    string           `json:"type"`
	Name    string           `json:"name"`
	Replies *QuestionReplies `json:"replies,omitempty"`
}

// QuestionReplies holds the vote tally for a poll option.
type QuestionReplies struct {
	Type       string `json:"type"`
	TotalItems int    `json:"totalItems"`
}

// Attachment represents media attached to a Note.
type Attachment struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	MediaType string `json:"mediaType,omitempty"`
	Name      string `json:"name,omitempty"` // alt text / description (AP "name" field)
	Blurhash  string `json:"blurhash,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

// Mention is a tag pointing to another actor.
type Mention struct {
	Type string `json:"type"`
	Href string `json:"href"`
	Name string `json:"name,omitempty"`
}

// Hashtag represents a hashtag tag on a Note.
type Hashtag struct {
	Type string `json:"type"`
	Href string `json:"href"`
	Name string `json:"name"`
}

// Emoji is a custom emoji tag.
type Emoji struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Icon *Image `json:"icon,omitempty"`
}

// Generator describes the application that created an object.
type Generator struct {
	Type string `json:"type"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Proxy links an AP object back to its Nostr origin.
type Proxy struct {
	Protocol      string `json:"protocol"`
	Proxied       string `json:"proxied"`
	Authoritative bool   `json:"authoritative,omitempty"`
}

// Activity is a generic ActivityPub activity.
type Activity struct {
	Context   interface{} `json:"@context,omitempty"`
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	Actor     string      `json:"actor"`
	Object    interface{} `json:"object"`
	To        []string    `json:"to,omitempty"`
	CC        []string    `json:"cc,omitempty"`
	Published string      `json:"published,omitempty"`
	ProxyOf   []Proxy     `json:"proxyOf,omitempty"`
}

// IncomingActivity is used for parsing incoming activities where the object
// might be a string reference or an embedded object.
type IncomingActivity struct {
	Context   interface{}     `json:"@context,omitempty"`
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Actor     string          `json:"actor"`
	Object    json.RawMessage `json:"object"`
	Target    json.RawMessage `json:"target,omitempty"` // used by Move activities
	To        StringOrArray   `json:"to,omitempty"`
	CC        StringOrArray   `json:"cc,omitempty"`
	Published string          `json:"published,omitempty"`
	Content   string          `json:"content,omitempty"`
}

// OrderedCollection is a paginated AP collection.
type OrderedCollection struct {
	Context      interface{} `json:"@context"`
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	TotalItems   int         `json:"totalItems"`
	OrderedItems interface{} `json:"orderedItems"`
}

// WebFinger response structures.
type WebFingerResponse struct {
	Subject string          `json:"subject"`
	Aliases []string        `json:"aliases,omitempty"`
	Links   []WebFingerLink `json:"links"`
}

type WebFingerLink struct {
	Rel      string `json:"rel"`
	Type     string `json:"type,omitempty"`
	Href     string `json:"href,omitempty"`
	Template string `json:"template,omitempty"`
}

// NodeInfo structures.
type NodeInfo struct {
	Version           string           `json:"version"`
	Software          NodeInfoSoftware `json:"software"`
	Protocols         []string         `json:"protocols"`
	Usage             NodeInfoUsage    `json:"usage"`
	OpenRegistrations bool             `json:"openRegistrations"`
}

type NodeInfoSoftware struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type NodeInfoUsage struct {
	Users NodeInfoUsers `json:"users"`
}

type NodeInfoUsers struct {
	Total          int `json:"total"`
	ActiveMonth    int `json:"activeMonth"`
	ActiveHalfYear int `json:"activeHalfYear"`
}

// WithContext wraps an object with the default AP @context.
func WithContext(v interface{}) map[string]interface{} {
	data, _ := json.Marshal(v)
	m := make(map[string]interface{})
	_ = json.Unmarshal(data, &m)
	m["@context"] = DefaultContext
	return m
}
