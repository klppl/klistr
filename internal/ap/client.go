package ap

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-fed/httpsig"
)

// ErrGone is returned when a remote resource responds with HTTP 410 Gone.
// This typically means the actor or object has been deleted.
var ErrGone = errors.New("resource gone (410)")

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

const objectCacheTTL = time.Hour

type cacheEntry struct {
	obj     map[string]interface{}
	expires time.Time
}

// objectCache is a TTL-bounded in-memory cache for fetched AP objects.
var objectCache sync.Map // url → cacheEntry

// FetchObject fetches an ActivityPub object from a remote URL.
// Returns the raw JSON or an error. Results are cached.
func FetchObject(ctx context.Context, rawURL string) (map[string]interface{}, error) {
	// Check cache first (skip if expired).
	if cached, ok := objectCache.Load(rawURL); ok {
		entry := cached.(cacheEntry)
		if time.Now().Before(entry.expires) {
			return entry.obj, nil
		}
		objectCache.Delete(rawURL)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", `application/activity+json, application/ld+json; profile="https://www.w3.org/ns/activitystreams"`)
	req.Header.Set("User-Agent", "klistr/1.0 (https://github.com/klppl/klistr)")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusGone {
		return nil, ErrGone
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}

	var obj map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode response from %s: %w", rawURL, err)
	}

	objectCache.Store(rawURL, cacheEntry{obj: obj, expires: time.Now().Add(objectCacheTTL)})
	return obj, nil
}

// FetchActor fetches and parses an AP Actor object.
func FetchActor(ctx context.Context, actorURL string) (*Actor, error) {
	obj, err := FetchObject(ctx, actorURL)
	if err != nil {
		return nil, err
	}
	return mapToActor(obj), nil
}

// InvalidateCache removes a URL from the object cache.
func InvalidateCache(rawURL string) {
	objectCache.Delete(rawURL)
}

// WebFingerResolve resolves a Fediverse handle (e.g. "alice@mastodon.social")
// to an AP actor URL via WebFinger. Returns the actor URL or an error.
func WebFingerResolve(ctx context.Context, handle string) (string, error) {
	parts := strings.SplitN(handle, "@", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid handle %q: expected user@domain", handle)
	}
	domain := parts[1]

	wfURL := "https://" + domain + "/.well-known/webfinger?resource=acct:" + handle

	req, err := http.NewRequestWithContext(ctx, "GET", wfURL, nil)
	if err != nil {
		return "", fmt.Errorf("webfinger request: %w", err)
	}
	req.Header.Set("Accept", "application/jrd+json, application/json")
	req.Header.Set("User-Agent", "klistr/1.0 (https://github.com/klppl/klistr)")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("webfinger fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webfinger returned HTTP %d for %s", resp.StatusCode, handle)
	}

	var wf struct {
		Links []struct {
			Rel  string `json:"rel"`
			Type string `json:"type"`
			Href string `json:"href"`
		} `json:"links"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wf); err != nil {
		return "", fmt.Errorf("webfinger decode: %w", err)
	}

	for _, link := range wf.Links {
		if link.Rel == "self" && (link.Type == "application/activity+json" ||
			link.Type == `application/ld+json; profile="https://www.w3.org/ns/activitystreams"`) {
			return link.Href, nil
		}
	}
	return "", fmt.Errorf("no ActivityPub actor link found for %s", handle)
}

// DeliverActivity sends an ActivityPub activity to a remote inbox using HTTP signatures.
func DeliverActivity(ctx context.Context, inbox string, activity map[string]interface{}, keyID string, privKey *rsa.PrivateKey) error {
	body, err := json.Marshal(activity)
	if err != nil {
		return fmt.Errorf("marshal activity: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", inbox, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("User-Agent", "klistr/1.0 (https://github.com/klppl/klistr)")
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("Host", req.URL.Host)

	// Sign the request.
	signer, _, err := httpsig.NewSigner(
		[]httpsig.Algorithm{httpsig.RSA_SHA256},
		httpsig.DigestSha256,
		[]string{httpsig.RequestTarget, "host", "date", "digest"},
		httpsig.Signature,
		0,
	)
	if err != nil {
		return fmt.Errorf("create signer: %w", err)
	}
	if err := signer.SignRequest(privKey, keyID, req, body); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("deliver to %s: %w", inbox, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("deliver to %s: HTTP %d", inbox, resp.StatusCode)
	}

	slog.Debug("delivered activity", "inbox", inbox, "status", resp.StatusCode)
	return nil
}

// VerifySignature verifies an incoming HTTP signature.
// Returns the keyID if valid, or an error.
func VerifySignature(req *http.Request) (string, error) {
	verifier, err := httpsig.NewVerifier(req)
	if err != nil {
		return "", fmt.Errorf("create verifier: %w", err)
	}

	keyID := verifier.KeyId()

	// Fetch the actor to get their public key.
	actorURL := strings.Split(keyID, "#")[0]
	actor, err := FetchActor(req.Context(), actorURL)
	if err != nil {
		if errors.Is(err, ErrGone) {
			// Actor has been deleted; signature cannot be verified but we
			// accept the activity (typically a Delete from a now-gone account).
			slog.Debug("actor gone, skipping signature verification", "keyId", keyID)
			return keyID, nil
		}
		return "", fmt.Errorf("fetch actor for key %s: %w", keyID, err)
	}

	if actor.PublicKey == nil {
		return "", fmt.Errorf("actor %s has no public key", actorURL)
	}

	pubKey, err := parsePublicKeyPEM(actor.PublicKey.PublicKeyPem)
	if err != nil {
		return "", fmt.Errorf("parse public key for %s: %w", actorURL, err)
	}

	if err := verifier.Verify(pubKey, httpsig.RSA_SHA256); err != nil {
		return "", fmt.Errorf("signature verification failed: %w", err)
	}

	return keyID, nil
}

func parsePublicKeyPEM(pemStr string) (*rsa.PublicKey, error) {
	// Use the same PEM parsing as keys.go
	block, _ := decodePEM([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("invalid PEM")
	}
	pub, err := parsePublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return pub, nil
}

// mapToActor extracts an Actor from a generic map.
func mapToActor(m map[string]interface{}) *Actor {
	if m == nil {
		return nil
	}
	actor := &Actor{
		ID:               getString(m, "id"),
		Type:             getString(m, "type"),
		Name:             getString(m, "name"),
		PreferredUsername: getString(m, "preferredUsername"),
		Summary:          getString(m, "summary"),
		Inbox:            getString(m, "inbox"),
		Outbox:           getString(m, "outbox"),
		Followers:        getString(m, "followers"),
		Following:        getString(m, "following"),
		URL:              getString(m, "url"),
	}

	// Extract publicKey
	if pk, ok := m["publicKey"].(map[string]interface{}); ok {
		actor.PublicKey = &PublicKey{
			ID:           getString(pk, "id"),
			Owner:        getString(pk, "owner"),
			PublicKeyPem: getString(pk, "publicKeyPem"),
		}
	}

	// Extract endpoints
	if ep, ok := m["endpoints"].(map[string]interface{}); ok {
		actor.Endpoints = &Endpoints{
			SharedInbox: getString(ep, "sharedInbox"),
		}
	}

	// Extract icon
	if icon, ok := m["icon"].(map[string]interface{}); ok {
		actor.Icon = &Image{
			Type: getString(icon, "type"),
			URL:  getString(icon, "url"),
		}
	}

	return actor
}

// mapToNote extracts a Note from a generic map.
func mapToNote(m map[string]interface{}) *Note {
	if m == nil {
		return nil
	}
	note := &Note{
		ID:           getString(m, "id"),
		Type:         getString(m, "type"),
		AttributedTo: getString(m, "attributedTo"),
		Content:      getString(m, "content"),
		Published:    getString(m, "published"),
		URL:          getString(m, "url"),
		InReplyTo:    getString(m, "inReplyTo"),
		QuoteURL:     getString(m, "quoteUrl"),
		Summary:      getString(m, "summary"),
	}
	if sens, ok := m["sensitive"].(bool); ok {
		note.Sensitive = sens
	}

	switch v := m["to"].(type) {
	case string:
		note.To = append(note.To, v)
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				note.To = append(note.To, s)
			}
		}
	}
	switch v := m["cc"].(type) {
	case string:
		note.CC = append(note.CC, v)
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				note.CC = append(note.CC, s)
			}
		}
	}

	// Extract tag array (mentions, hashtags, emoji).
	if tags, ok := m["tag"].([]interface{}); ok {
		note.Tag = tags
	}

	// Extract media attachments.
	if atts, ok := m["attachment"].([]interface{}); ok {
		for _, att := range atts {
			a, ok := att.(map[string]interface{})
			if !ok {
				continue
			}
			width, _ := a["width"].(float64)
			height, _ := a["height"].(float64)
			note.Attachment = append(note.Attachment, Attachment{
				Type:      getString(a, "type"),
				URL:       getString(a, "url"),
				MediaType: getString(a, "mediaType"),
				Blurhash:  getString(a, "blurhash"),
				Width:     int(width),
				Height:    int(height),
			})
		}
	}

	return note
}

// IsActor returns true if the object type is an actor type.
func IsActor(obj map[string]interface{}) bool {
	t := getString(obj, "type")
	switch t {
	case "Person", "Service", "Application", "Group", "Organization":
		return true
	}
	return false
}

// IsLocalID returns true if the AP ID belongs to our local domain.
func IsLocalID(apID, localDomain string) bool {
	base := strings.TrimRight(localDomain, "/")
	return apID == base || strings.HasPrefix(apID, base+"/")
}

// IsActorID returns true if the ID looks like an AP actor URL.
func IsActorID(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
