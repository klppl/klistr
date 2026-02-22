package nostr

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// EventHandler is a function that processes a Nostr event.
type EventHandler func(ctx context.Context, event *nostr.Event)

// RelayPool manages connections to one or more Nostr relays and subscribes
// to events from a single author (the configured local Nostr user).
type RelayPool struct {
	readRelays   []string
	writeRelays  []string
	authorPubKey string
	handler      EventHandler
	sem          chan struct{} // limits concurrent event-handler goroutines
}

// relayEventConcurrency is the maximum number of event-handler goroutines that
// may run simultaneously. Events arriving while all slots are occupied are
// dropped with a warning rather than spawning unbounded goroutines.
const relayEventConcurrency = 20

// NewRelayPool creates a relay pool that subscribes to events from authorPubKey.
func NewRelayPool(readRelays, writeRelays []string, authorPubKey string, handler EventHandler) *RelayPool {
	return &RelayPool{
		readRelays:   readRelays,
		writeRelays:  writeRelays,
		authorPubKey: authorPubKey,
		handler:      handler,
		sem:          make(chan struct{}, relayEventConcurrency),
	}
}

// Start begins listening to the relay firehose. This blocks until ctx is cancelled.
// It subscribes to events from the local author from now onwards.
func (rp *RelayPool) Start(ctx context.Context) {
	if len(rp.readRelays) == 0 {
		slog.Warn("no read relays configured; relay firehose is disabled")
		<-ctx.Done()
		return
	}

	slog.Info("starting relay firehose", "relays", rp.readRelays, "author", rp.authorPubKey[:8])

	pool := nostr.NewSimplePool(ctx)
	since := nostr.Now()

	filters := nostr.Filters{{
		Kinds:   []int{0, 1, 3, 5, 6, 7, 9735},
		Authors: []string{rp.authorPubKey},
		Since:   &since,
		Limit:   0,
	}}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		slog.Debug("subscribing to relay firehose", "relays", rp.readRelays)

		for ev := range pool.SubMany(ctx, rp.readRelays, filters) {
			if ev.Event == nil {
				continue
			}
			// Process event in a goroutine to avoid blocking the subscription.
			// The semaphore caps concurrency; events that arrive while all slots
			// are occupied are dropped with a warning instead of piling up.
			event := ev.Event
			select {
			case rp.sem <- struct{}{}:
				go func() {
					defer func() { <-rp.sem }()
					defer func() {
						if r := recover(); r != nil {
							slog.Error("panic in event handler", "panic", r)
						}
					}()
					rp.handler(ctx, event)
				}()
			default:
				slog.Warn("relay event dropped: handler backlog full", "id", event.ID)
			}
		}

		// If we exit the loop (relay disconnect), wait and reconnect.
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			slog.Info("reconnecting to relay firehose")
			since = nostr.Now()
			filters[0].Since = &since
		}
	}
}

// Publisher publishes Nostr events to write relays.
type Publisher struct {
	writeRelays []string
	pool        *nostr.SimplePool
	poolOnce    sync.Once
}

// NewPublisher creates a new Publisher.
func NewPublisher(writeRelays []string) *Publisher {
	return &Publisher{writeRelays: writeRelays}
}

// getPool returns the shared, lazily-initialised relay pool.
func (p *Publisher) getPool() *nostr.SimplePool {
	p.poolOnce.Do(func() {
		p.pool = nostr.NewSimplePool(context.Background())
	})
	return p.pool
}

// Publish publishes an event to all configured write relays.
// A fresh 15-second timeout is used for each publish attempt, decoupled from
// the caller's context. This prevents short-lived or already-cancelled contexts
// (HTTP request contexts, per-operation timeouts) from aborting relay delivery.
func (p *Publisher) Publish(ctx context.Context, event *nostr.Event) error {
	if len(p.writeRelays) == 0 {
		slog.Warn("no write relays configured; event not published", "id", event.ID, "kind", event.Kind)
		return nil
	}

	// Honour explicit cancellation (e.g. SIGTERM) but otherwise use an
	// independent deadline so relay reconnect + handshake time doesn't cause
	// spurious "context canceled" failures.
	publishCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Propagate explicit cancellation from the caller.
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-publishCtx.Done():
		}
	}()

	results := p.getPool().PublishMany(publishCtx, p.writeRelays, *event)

	var published, failed int
	for result := range results {
		if result.Error != nil {
			slog.Warn("failed to publish event", "relay", result.RelayURL, "id", event.ID, "error", result.Error)
			failed++
		} else {
			slog.Debug("published event", "relay", result.RelayURL, "id", event.ID, "kind", event.Kind)
			published++
		}
	}

	if published == 0 && failed > 0 {
		return fmt.Errorf("failed to publish to all %d relays", failed)
	}
	return nil
}
