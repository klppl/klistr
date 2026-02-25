package bsky

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultPDSURL = "https://bsky.social"

// Client is a thin XRPC HTTP client for the Bluesky PDS.
// It handles authentication and re-authenticates automatically on 401.
type Client struct {
	PDSURL      string
	Identifier  string
	AppPassword string

	mu                 sync.Mutex
	session            *Session
	http               *http.Client
	rateLimitRemaining int
	rateLimitReset     time.Time

	// reauth serialises re-authentication attempts so that concurrent goroutines
	// (e.g. poller + poster) that both receive a 401 don't each independently call
	// createSession — which would cause each new session to immediately invalidate
	// the previous one (thundering herd on the token endpoint).
	reauth sync.Mutex
}

// rateLimitWarnThreshold is the RateLimit-Remaining value below which we emit
// a warning so operators notice before requests start failing.
const rateLimitWarnThreshold = 10

// rateLimitRetryMax caps how long we'll sleep after a 429 before retrying.
const rateLimitRetryMax = 5 * time.Minute

// errRateLimited is returned by doRequest when the PDS responds with HTTP 429.
type errRateLimited struct {
	RetryAfter time.Duration
}

func (e *errRateLimited) Error() string {
	return fmt.Sprintf("rate limited by Bluesky PDS; retry after %s", e.RetryAfter.Round(time.Second))
}

// parseRetryAfter derives how long to wait from the 429 response headers.
// It checks Retry-After (seconds integer) first, then RateLimit-Reset (unix ts).
func parseRetryAfter(resp *http.Response) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	if s := resp.Header.Get("RateLimit-Reset"); s != "" {
		if ts, err := strconv.ParseInt(s, 10, 64); err == nil {
			if d := time.Until(time.Unix(ts, 0)); d > 0 {
				return d
			}
		}
	}
	return 30 * time.Second // sane default when headers are absent
}

// NewClient creates a new Bluesky XRPC client.
func NewClient(identifier, appPassword string) *Client {
	return &Client{
		PDSURL:      defaultPDSURL,
		Identifier:  identifier,
		AppPassword: appPassword,
		http: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Authenticate creates a new session via com.atproto.server.createSession.
// Must be called before any other operations.
func (c *Client) Authenticate(ctx context.Context) error {
	input := CreateSessionInput{
		Identifier: c.Identifier,
		Password:   c.AppPassword,
	}
	var session Session
	if err := c.xrpcPost(ctx, "com.atproto.server.createSession", input, &session); err != nil {
		return fmt.Errorf("bsky authenticate: %w", err)
	}
	c.mu.Lock()
	c.session = &session
	c.mu.Unlock()
	slog.Info("bsky authenticated", "did", session.DID, "handle", session.Handle)
	return nil
}

// singleAuthenticate refreshes the session exactly once per expired token.
//
// staleToken is the AccessJwt that was in use when the 401 was received.
// If another goroutine already refreshed the session by the time this goroutine
// acquires the reauth mutex (current JWT ≠ staleToken), we skip the API call
// and return nil — the caller will retry with the already-fresh token.
//
// This prevents the thundering-herd pattern where N goroutines each call
// createSession concurrently, each new session invalidating the last one.
func (c *Client) singleAuthenticate(ctx context.Context, staleToken string) error {
	c.reauth.Lock()
	defer c.reauth.Unlock()

	// Check whether another goroutine has already refreshed the token while we
	// were waiting for the mutex.
	c.mu.Lock()
	var current string
	if c.session != nil {
		current = c.session.AccessJwt
	}
	c.mu.Unlock()

	if staleToken != "" && current != staleToken {
		// Token was already refreshed — nothing to do.
		return nil
	}

	slog.Warn("bsky token expired, re-authenticating")
	return c.Authenticate(ctx)
}

// CreateRecord creates a record via com.atproto.repo.createRecord.
func (c *Client) CreateRecord(ctx context.Context, req CreateRecordRequest) (*CreateRecordResponse, error) {
	var resp CreateRecordResponse
	if err := c.authedPost(ctx, "com.atproto.repo.createRecord", req, &resp); err != nil {
		return nil, fmt.Errorf("bsky createRecord: %w", err)
	}
	return &resp, nil
}

// DeleteRecord deletes a record via com.atproto.repo.deleteRecord.
func (c *Client) DeleteRecord(ctx context.Context, repo, collection, rkey string) error {
	req := DeleteRecordRequest{
		Repo:       repo,
		Collection: collection,
		RKey:       rkey,
	}
	if err := c.authedPost(ctx, "com.atproto.repo.deleteRecord", req, nil); err != nil {
		return fmt.Errorf("bsky deleteRecord: %w", err)
	}
	return nil
}

// ListNotifications fetches notifications from app.bsky.notification.listNotifications.
// Pass an empty cursor to start from the beginning.
func (c *Client) ListNotifications(ctx context.Context, cursor string) (*ListNotificationsResponse, error) {
	params := url.Values{}
	params.Set("limit", "50")
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	var resp ListNotificationsResponse
	if err := c.authedGet(ctx, "app.bsky.notification.listNotifications", params, &resp); err != nil {
		return nil, fmt.Errorf("bsky listNotifications: %w", err)
	}
	return &resp, nil
}

// GetTimeline fetches the home timeline (posts from followed accounts) via
// app.bsky.feed.getTimeline. Returns at most 50 items, newest first.
func (c *Client) GetTimeline(ctx context.Context) (*GetTimelineResponse, error) {
	params := url.Values{}
	params.Set("limit", "50")
	var resp GetTimelineResponse
	if err := c.authedGet(ctx, "app.bsky.feed.getTimeline", params, &resp); err != nil {
		return nil, fmt.Errorf("bsky getTimeline: %w", err)
	}
	return &resp, nil
}

// FollowActor creates a follow record for the given DID via app.bsky.graph.follow.
// Returns the rkey of the created record (used for later deletion).
func (c *Client) FollowActor(ctx context.Context, did string) (string, error) {
	req := CreateRecordRequest{
		Repo:       c.DID(),
		Collection: "app.bsky.graph.follow",
		Record: map[string]interface{}{
			"$type":     "app.bsky.graph.follow",
			"subject":   did,
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		},
	}
	resp, err := c.CreateRecord(ctx, req)
	if err != nil {
		return "", fmt.Errorf("bsky followActor: %w", err)
	}
	// URI format: at://did:plc:xxx/app.bsky.graph.follow/<rkey>
	parts := strings.Split(resp.URI, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("bsky followActor: unexpected URI format: %s", resp.URI)
	}
	return parts[len(parts)-1], nil
}

// GetPostThread fetches the thread view for a post, including up to 10 levels
// of ancestor posts and no replies (depth=0). Used to bridge missing parent
// posts when a followed account replies inside a thread.
func (c *Client) GetPostThread(ctx context.Context, uri string) (*GetPostThreadResponse, error) {
	params := url.Values{}
	params.Set("uri", uri)
	params.Set("depth", "0")
	params.Set("parentHeight", "10")
	var resp GetPostThreadResponse
	if err := c.authedGet(ctx, "app.bsky.feed.getPostThread", params, &resp); err != nil {
		return nil, fmt.Errorf("bsky getPostThread: %w", err)
	}
	return &resp, nil
}

// GetProfile fetches a profile via app.bsky.actor.getProfile.
func (c *Client) GetProfile(ctx context.Context, actor string) (*Profile, error) {
	params := url.Values{}
	params.Set("actor", actor)
	var resp Profile
	if err := c.authedGet(ctx, "app.bsky.actor.getProfile", params, &resp); err != nil {
		return nil, fmt.Errorf("bsky getProfile: %w", err)
	}
	return &resp, nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// errAuthExpired is returned by doRequest when the PDS signals that the
// current access token is no longer valid (HTTP 401 or ExpiredToken body).
var errAuthExpired = errors.New("auth expired")

// isAuthError returns true for errors that indicate a stale/expired token.
func isAuthError(err error) bool {
	return errors.Is(err, errAuthExpired)
}

// authedPost performs an authenticated XRPC POST, re-authenticating on auth
// errors and backing off on rate-limit responses.
func (c *Client) authedPost(ctx context.Context, method string, body, out interface{}) error {
	// Capture the token in use before the request so singleAuthenticate can
	// detect whether another goroutine has already refreshed it.
	staleToken := c.currentToken()

	err := c.xrpcPostWithAuth(ctx, method, body, out)
	if isAuthError(err) {
		if authErr := c.singleAuthenticate(ctx, staleToken); authErr != nil {
			return fmt.Errorf("re-authenticate: %w", authErr)
		}
		err = c.xrpcPostWithAuth(ctx, method, body, out)
	}
	var rl *errRateLimited
	if errors.As(err, &rl) {
		wait := rl.RetryAfter
		if wait > rateLimitRetryMax {
			wait = rateLimitRetryMax
		}
		slog.Warn("bsky rate limited on POST, backing off", "method", method, "retry_after", wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		err = c.xrpcPostWithAuth(ctx, method, body, out)
	}
	return err
}

// authedGet performs an authenticated XRPC GET, re-authenticating on auth
// errors and backing off on rate-limit responses.
func (c *Client) authedGet(ctx context.Context, method string, params url.Values, out interface{}) error {
	// Capture the token in use before the request so singleAuthenticate can
	// detect whether another goroutine has already refreshed it.
	staleToken := c.currentToken()

	err := c.xrpcGetWithAuth(ctx, method, params, out)
	if isAuthError(err) {
		if authErr := c.singleAuthenticate(ctx, staleToken); authErr != nil {
			return fmt.Errorf("re-authenticate: %w", authErr)
		}
		err = c.xrpcGetWithAuth(ctx, method, params, out)
	}
	var rl *errRateLimited
	if errors.As(err, &rl) {
		wait := rl.RetryAfter
		if wait > rateLimitRetryMax {
			wait = rateLimitRetryMax
		}
		slog.Warn("bsky rate limited on GET, backing off", "method", method, "retry_after", wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		err = c.xrpcGetWithAuth(ctx, method, params, out)
	}
	return err
}

// xrpcPost sends a POST to the XRPC endpoint without auth headers.
// Used only for createSession itself.
func (c *Client) xrpcPost(ctx context.Context, method string, body, out interface{}) error {
	return c.doPost(ctx, method, body, out, "")
}

// xrpcPostWithAuth sends an authenticated POST.
func (c *Client) xrpcPostWithAuth(ctx context.Context, method string, body, out interface{}) error {
	return c.doPost(ctx, method, body, out, c.authHeader())
}

// xrpcGetWithAuth sends an authenticated GET.
func (c *Client) xrpcGetWithAuth(ctx context.Context, method string, params url.Values, out interface{}) error {
	rawURL := c.PDSURL + "/xrpc/" + method
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("create GET request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "klistr/1.0 (https://github.com/klppl/klistr)")
	if auth := c.authHeader(); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	return c.doRequest(req, out)
}

func (c *Client) doPost(ctx context.Context, method string, body interface{}, out interface{}, authHeader string) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	rawURL := c.PDSURL + "/xrpc/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create POST request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "klistr/1.0 (https://github.com/klppl/klistr)")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	return c.doRequest(req, out)
}

// updateRateLimit records the RateLimit-Remaining / RateLimit-Reset headers
// from any successful response and warns when headroom is critically low.
func (c *Client) updateRateLimit(resp *http.Response) {
	s := resp.Header.Get("RateLimit-Remaining")
	if s == "" {
		return
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return
	}
	var reset time.Time
	if rs := resp.Header.Get("RateLimit-Reset"); rs != "" {
		if ts, err := strconv.ParseInt(rs, 10, 64); err == nil {
			reset = time.Unix(ts, 0)
		}
	}
	c.mu.Lock()
	c.rateLimitRemaining = n
	c.rateLimitReset = reset
	c.mu.Unlock()
	if n <= rateLimitWarnThreshold {
		slog.Warn("bsky rate limit headroom low",
			"remaining", n,
			"reset_in", time.Until(reset).Round(time.Second),
		)
	}
}

func (c *Client) doRequest(req *http.Request, out interface{}) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	// Track rate-limit headroom on every response (header absent on error codes too).
	c.updateRateLimit(resp)

	if resp.StatusCode == 401 {
		return errAuthExpired
	}
	if resp.StatusCode == 400 && strings.Contains(string(respBody), "ExpiredToken") {
		return errAuthExpired
	}
	if resp.StatusCode == 429 {
		return &errRateLimited{RetryAfter: parseRetryAfter(resp)}
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// authHeader returns the Bearer token header value from the current session.
func (c *Client) authHeader() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return ""
	}
	return "Bearer " + c.session.AccessJwt
}

// currentToken returns the raw AccessJwt from the current session, or empty
// string if not authenticated. Used by authedGet/authedPost to capture the
// stale token before a request so singleAuthenticate can skip a redundant
// createSession call when another goroutine has already refreshed the token.
func (c *Client) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return ""
	}
	return c.session.AccessJwt
}

// DID returns the authenticated user's DID, or empty string if not authenticated.
func (c *Client) DID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return ""
	}
	return c.session.DID
}

// Handle returns the authenticated user's handle, or empty string if not authenticated.
func (c *Client) Handle() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return ""
	}
	return c.session.Handle
}
