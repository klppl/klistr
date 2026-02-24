package ap

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
)

// Federator handles outbound federation of AP activities.
type Federator struct {
	LocalDomain string
	KeyID       string // e.g. "https://example.com/actor#main-key"
	PrivateKey  *rsa.PrivateKey
	// GetFollowers returns AP follower IDs for a local AP actor URL.
	GetFollowers func(actorURL string) ([]string, error)
}

// federationConcurrency caps the number of concurrent outbound HTTP requests
// used both for actor fetches (resolveInboxes) and activity delivery (Federate).
const federationConcurrency = 10

// Federate distributes an activity to all relevant inboxes.
// It resolves follower lists, fetches actor inboxes, and delivers via HTTP.
func (f *Federator) Federate(ctx context.Context, activity map[string]interface{}) {
	id, _ := activity["id"].(string)
	activityType, _ := activity["type"].(string)

	recipients := f.collectRecipients(ctx, activity)
	inboxes := f.resolveInboxes(ctx, recipients)

	slog.Debug("federating activity",
		"id", id,
		"type", activityType,
		"inboxes", len(inboxes),
	)

	// Deliver to all inboxes in parallel, bounded to avoid overwhelming remote
	// servers and exhausting local resources during large fan-outs.
	sem := make(chan struct{}, federationConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var success, failed int

	for inbox := range inboxes {
		sem <- struct{}{}
		wg.Add(1)
		go func(inbox string) {
			defer func() { <-sem; wg.Done() }()
			if err := DeliverActivity(ctx, inbox, activity, f.KeyID, f.PrivateKey); err != nil {
				slog.Warn("federation failed", "inbox", inbox, "error", err)
				mu.Lock()
				failed++
				mu.Unlock()
			} else {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(inbox)
	}
	wg.Wait()

	slog.Debug("federation complete",
		"id", id,
		"type", activityType,
		"success", success,
		"failed", failed,
	)
}

// collectRecipients gathers all recipient IDs from the activity's to/cc fields,
// expanding follower collections.
func (f *Federator) collectRecipients(ctx context.Context, activity map[string]interface{}) map[string]struct{} {
	recipients := make(map[string]struct{})

	addList := func(key string) {
		if list, ok := activity[key].([]interface{}); ok {
			for _, v := range list {
				if s, ok := v.(string); ok {
					recipients[s] = struct{}{}
				}
			}
		}
		if list, ok := activity[key].([]string); ok {
			for _, s := range list {
				recipients[s] = struct{}{}
			}
		}
	}

	addList("to")
	addList("cc")

	// Expand followers collections.
	actorID, _ := activity["actor"].(string)
	followersCollection := actorID + "/followers"

	if _, ok := recipients[followersCollection]; ok {
		delete(recipients, followersCollection)

		if f.GetFollowers != nil {
			followers, err := f.GetFollowers(actorID)
			if err != nil {
				slog.Warn("failed to get followers", "actor", actorID, "error", err)
			} else {
				for _, follower := range followers {
					recipients[follower] = struct{}{}
				}
			}
		}
	}

	return recipients
}

// resolveInboxes converts recipient IDs to inbox URLs, deduplicating by origin.
// Actor fetches are performed concurrently (bounded by federationConcurrency)
// so a large follower list doesn't serialize into N sequential 10s HTTP calls.
func (f *Federator) resolveInboxes(ctx context.Context, recipients map[string]struct{}) map[string]struct{} {
	// Filter to only the IDs that need an outbound fetch.
	var toResolve []string
	for recipientID := range recipients {
		if !IsActorID(recipientID) || IsLocalID(recipientID, f.LocalDomain) || recipientID == PublicURI {
			continue
		}
		toResolve = append(toResolve, recipientID)
	}

	var (
		mu      sync.Mutex
		inboxes = make(map[string]struct{})
		seen    = make(map[string]struct{}) // origins already covered by a shared inbox
		sem     = make(chan struct{}, federationConcurrency)
		wg      sync.WaitGroup
	)

	for _, recipientID := range toResolve {
		sem <- struct{}{}
		wg.Add(1)
		go func(id string) {
			defer func() { <-sem; wg.Done() }()

			actor, err := FetchActor(ctx, id)
			if err != nil {
				slog.Debug("failed to fetch actor for federation", "actor", id, "error", err)
				return
			}

			inbox := actor.Inbox
			if actor.Endpoints != nil && actor.Endpoints.SharedInbox != "" {
				// Use shared inbox, but only once per origin to avoid
				// delivering the same activity multiple times to one server.
				origin := extractOrigin(actor.Endpoints.SharedInbox)
				mu.Lock()
				_, already := seen[origin]
				if !already {
					seen[origin] = struct{}{}
				}
				mu.Unlock()
				if already {
					return // another goroutine already claimed this origin
				}
				inbox = actor.Endpoints.SharedInbox
			}

			if inbox != "" {
				mu.Lock()
				inboxes[inbox] = struct{}{}
				mu.Unlock()
			}
		}(recipientID)
	}

	wg.Wait()
	return inboxes
}

// GetActorInbox returns the inbox URL for an AP actor.
func GetActorInbox(ctx context.Context, actorID string) (string, error) {
	actor, err := FetchActor(ctx, actorID)
	if err != nil {
		return "", err
	}
	if actor.Endpoints != nil && actor.Endpoints.SharedInbox != "" {
		return actor.Endpoints.SharedInbox, nil
	}
	return actor.Inbox, nil
}

// ActivityToMap converts an Activity struct to a generic map for sending.
func ActivityToMap(v interface{}) map[string]interface{} {
	data, _ := json.Marshal(v)
	m := make(map[string]interface{})
	_ = json.Unmarshal(data, &m)
	m["@context"] = DefaultContext
	return m
}

func extractOrigin(rawURL string) string {
	if idx := strings.Index(rawURL, "://"); idx != -1 {
		rest := rawURL[idx+3:]
		if slash := strings.IndexByte(rest, '/'); slash != -1 {
			return rawURL[:idx+3+slash]
		}
		return rawURL
	}
	return rawURL
}
