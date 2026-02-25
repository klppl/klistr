package nostr

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"golang.org/x/time/rate"
)

// EventHandler is a function that processes a Nostr event.
type EventHandler func(ctx context.Context, event *nostr.Event)

const (
	cbCooldown            = 5 * time.Minute
	relayEventConcurrency = 20
)

// cbThreshold is a var (not const) so it can be overridden at startup via
// SetCircuitBreakerThreshold for deployments that need a different sensitivity.
var cbThreshold = 3 // consecutive failures before circuit opens

// SetCircuitBreakerThreshold sets the number of consecutive publish failures
// required before a relay's circuit breaker opens. Call once at startup,
// before any Publisher is created, to override the default of 3.
func SetCircuitBreakerThreshold(n int) {
	if n > 0 {
		cbThreshold = n
	}
}

// relayCircuit is a per-relay circuit breaker.
type relayCircuit struct {
	mu            sync.Mutex
	failCount     int
	openedAt      time.Time
	open          bool
	permanentOpen bool // true when relay requires PoW; stays open until manual reset
}

// isOpen returns true when the circuit is open (relay should be bypassed).
// Resets to closed once cbCooldown has elapsed (half-open retry), unless permanentOpen is set.
func (cb *relayCircuit) isOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.permanentOpen {
		return true
	}
	if !cb.open {
		return false
	}
	if time.Since(cb.openedAt) >= cbCooldown {
		cb.open = false
		cb.failCount = 0
		return false
	}
	return true
}

// openForPoW permanently opens the circuit for a relay that requires proof-of-work.
// The circuit stays open until reset() is called (e.g. via the admin UI).
func (cb *relayCircuit) openForPoW() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.open = true
	cb.permanentOpen = true
	cb.openedAt = time.Now()
	cb.failCount = cbThreshold
}

// recordFailure increments the counter and opens the circuit at threshold.
// Returns true the first time the circuit opens.
func (cb *relayCircuit) recordFailure() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failCount++
	if !cb.open && cb.failCount >= cbThreshold {
		cb.open = true
		cb.openedAt = time.Now()
		return true
	}
	return false
}

// recordSuccess resets all failure state. Returns true if the circuit was open.
func (cb *relayCircuit) recordSuccess() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	was := cb.open || cb.failCount > 0
	cb.open = false
	cb.failCount = 0
	return was
}

// reset forcefully clears the circuit breaker state, including any permanent PoW lock.
func (cb *relayCircuit) reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.open = false
	cb.permanentOpen = false
	cb.failCount = 0
}

// RelayStatus describes a relay and its circuit-breaker state.
type RelayStatus struct {
	URL               string
	CircuitOpen       bool
	FailCount         int
	CooldownRemaining int // seconds remaining until circuit resets
}

func (cb *relayCircuit) status(url string) RelayStatus {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	open := cb.permanentOpen || (cb.open && time.Since(cb.openedAt) < cbCooldown)
	var remaining int
	if open && !cb.permanentOpen {
		r := cbCooldown - time.Since(cb.openedAt)
		if r > 0 {
			remaining = int(r.Seconds())
		}
	}
	return RelayStatus{
		URL:               url,
		CircuitOpen:       open,
		FailCount:         cb.failCount,
		CooldownRemaining: remaining,
	}
}

// ─── RelayPool ────────────────────────────────────────────────────────────────

// RelayPool manages read-relay subscriptions for a single Nostr author.
type RelayPool struct {
	mu           sync.RWMutex
	readRelays   []string
	authorPubKey string
	handler      EventHandler
	sem          chan struct{}
	restartCh    chan struct{} // closed/sent when relay list changes
}

// NewRelayPool creates a relay pool that subscribes to events from authorPubKey.
// The writeRelays parameter is accepted for API compatibility but unused (Publisher handles writes).
func NewRelayPool(readRelays, _ []string, authorPubKey string, handler EventHandler) *RelayPool {
	return &RelayPool{
		readRelays:   append([]string{}, readRelays...),
		authorPubKey: authorPubKey,
		handler:      handler,
		sem:          make(chan struct{}, relayEventConcurrency),
		restartCh:    make(chan struct{}, 1),
	}
}

// AddRelay adds a relay to the read list and triggers an immediate subscription restart.
// Returns false if the relay is already present.
func (rp *RelayPool) AddRelay(url string) bool {
	rp.mu.Lock()
	for _, r := range rp.readRelays {
		if r == url {
			rp.mu.Unlock()
			return false
		}
	}
	rp.readRelays = append(rp.readRelays, url)
	rp.mu.Unlock()
	select {
	case rp.restartCh <- struct{}{}:
	default:
	}
	return true
}

// RemoveRelay removes a relay from the read list and triggers a restart.
// Returns false if the relay was not in the list.
func (rp *RelayPool) RemoveRelay(url string) bool {
	rp.mu.Lock()
	for i, r := range rp.readRelays {
		if r == url {
			rp.readRelays = append(rp.readRelays[:i], rp.readRelays[i+1:]...)
			rp.mu.Unlock()
			select {
			case rp.restartCh <- struct{}{}:
			default:
			}
			return true
		}
	}
	rp.mu.Unlock()
	return false
}

// Relays returns a copy of the current read relay list.
func (rp *RelayPool) Relays() []string {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return append([]string{}, rp.readRelays...)
}

// Start begins listening to the relay firehose. Blocks until ctx is cancelled.
func (rp *RelayPool) Start(ctx context.Context) {
	rp.mu.RLock()
	empty := len(rp.readRelays) == 0
	rp.mu.RUnlock()
	if empty {
		slog.Warn("no read relays configured; relay firehose is disabled")
		<-ctx.Done()
		return
	}

	pool := nostr.NewSimplePool(ctx)
	since := nostr.Now()

	for {
		rp.mu.RLock()
		relays := append([]string{}, rp.readRelays...)
		rp.mu.RUnlock()

		select {
		case <-ctx.Done():
			return
		default:
		}

		slog.Info("starting relay firehose", "relays", relays, "author", rp.authorPubKey[:8])

		filters := nostr.Filters{{
			Kinds:   []int{0, 1, 3, 5, 6, 7, 1068, 9735, 10002, 30023},
			Authors: []string{rp.authorPubKey},
			Since:   &since,
			Limit:   0,
		}}

		subCtx, subCancel := context.WithCancel(ctx)
		immediateRestart := make(chan struct{}, 1)

		// Forward relay-list-change signals to immediateRestart and cancel the subscription.
		go func() {
			select {
			case <-rp.restartCh:
				select {
				case immediateRestart <- struct{}{}:
				default:
				}
				subCancel()
			case <-subCtx.Done():
			}
		}()

		for ev := range pool.SubMany(subCtx, relays, filters) {
			if ev.Event == nil {
				continue
			}
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
		subCancel()

		select {
		case <-ctx.Done():
			return
		default:
		}

		// Relay list changed: restart immediately without the reconnect delay.
		select {
		case <-immediateRestart:
			slog.Info("relay list updated, resubscribing", "relays", rp.Relays())
			since = nostr.Now()
			continue
		default:
		}

		// Normal connection drop: wait before reconnecting.
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			slog.Info("reconnecting to relay firehose")
			since = nostr.Now()
		}
	}
}

// ─── Publisher ────────────────────────────────────────────────────────────────

// Publisher publishes Nostr events to write relays with per-relay circuit breakers.
// A circuit opens after cbThreshold consecutive failures and stays open for cbCooldown,
// preventing repeated connection attempts to unreachable relays.
type Publisher struct {
	mu       sync.RWMutex
	relays   []string
	circuits map[string]*relayCircuit
	pool     *nostr.SimplePool
	poolOnce sync.Once
	limiter  *rate.Limiter
}

const (
	publishRateLimit = rate.Limit(2) // 2 events per second per publisher
	publishRateBurst = 5             // burst allowance to handle short threads
)

// NewPublisher creates a new Publisher.
func NewPublisher(writeRelays []string) *Publisher {
	circuits := make(map[string]*relayCircuit, len(writeRelays))
	for _, r := range writeRelays {
		circuits[r] = &relayCircuit{}
	}
	return &Publisher{
		relays:   append([]string{}, writeRelays...),
		circuits: circuits,
		limiter:  rate.NewLimiter(publishRateLimit, publishRateBurst),
	}
}

// AddRelay adds a relay to the write list. Returns false if already present.
func (p *Publisher) AddRelay(url string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.relays {
		if r == url {
			return false
		}
	}
	p.relays = append(p.relays, url)
	p.circuits[url] = &relayCircuit{}
	return true
}

// RemoveRelay removes a relay from the write list. Returns false if not found.
func (p *Publisher) RemoveRelay(url string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, r := range p.relays {
		if r == url {
			p.relays = append(p.relays[:i], p.relays[i+1:]...)
			delete(p.circuits, url)
			return true
		}
	}
	return false
}

// Relays returns a copy of the current write relay list.
func (p *Publisher) Relays() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string{}, p.relays...)
}

// RelayStatuses returns the circuit-breaker state for all configured relays.
func (p *Publisher) RelayStatuses() []RelayStatus {
	p.mu.RLock()
	relays := append([]string{}, p.relays...)
	circuits := make(map[string]*relayCircuit, len(p.circuits))
	for k, v := range p.circuits {
		circuits[k] = v
	}
	p.mu.RUnlock()

	statuses := make([]RelayStatus, 0, len(relays))
	for _, url := range relays {
		if cb, ok := circuits[url]; ok {
			statuses = append(statuses, cb.status(url))
		} else {
			statuses = append(statuses, RelayStatus{URL: url})
		}
	}
	return statuses
}

// ResetCircuit clears the circuit-breaker state for a specific relay.
func (p *Publisher) ResetCircuit(url string) {
	p.mu.RLock()
	cb := p.circuits[url]
	p.mu.RUnlock()
	if cb != nil {
		cb.reset()
		slog.Info("relay circuit breaker reset", "relay", url)
	}
}

// getCircuit returns or creates a circuit breaker for the given relay URL.
func (p *Publisher) getCircuit(url string) *relayCircuit {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cb, ok := p.circuits[url]; ok {
		return cb
	}
	cb := &relayCircuit{}
	p.circuits[url] = cb
	return cb
}

// getPool returns the shared, lazily-initialised SimplePool.
func (p *Publisher) getPool() *nostr.SimplePool {
	p.poolOnce.Do(func() {
		p.pool = nostr.NewSimplePool(context.Background())
	})
	return p.pool
}

// Publish publishes an event to all configured write relays.
// Relays with open circuits are skipped. If at least one relay succeeds, no error is returned.
// An independent 15-second timeout is used so short-lived caller contexts don't abort delivery.
func (p *Publisher) Publish(ctx context.Context, event *nostr.Event) error {
	p.mu.RLock()
	allRelays := append([]string{}, p.relays...)
	p.mu.RUnlock()

	if len(allRelays) == 0 {
		slog.Warn("no write relays configured; event not published", "id", event.ID, "kind", event.Kind)
		return nil
	}

	// Skip relays with open circuits to avoid hammering unreachable endpoints.
	active := make([]string, 0, len(allRelays))
	for _, url := range allRelays {
		if p.getCircuit(url).isOpen() {
			slog.Debug("skipping relay with open circuit", "relay", url, "id", event.ID)
		} else {
			active = append(active, url)
		}
	}

	if len(active) == 0 {
		slog.Warn("all relay circuits are open; event not published",
			"id", event.ID, "skipped", len(allRelays))
		return fmt.Errorf("all %d relays have open circuits", len(allRelays))
	}

	// Wait for an outbound rate limit token so we don't trip anti-spam
	// circuits on strict relays (e.g. relay.damus.io) during sync bursts.
	if err := p.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("outbound rate limit wait: %w", err)
	}

	// Honour explicit cancellation but otherwise use an independent deadline.
	publishCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-publishCtx.Done():
		}
	}()

	var published, failed int
	for result := range p.getPool().PublishMany(publishCtx, active, *event) {
		cb := p.getCircuit(result.RelayURL)
		if result.Error != nil {
			if isPowRequired(result.Error) {
				// Relay requires NIP-13 proof-of-work which klistr doesn't mine.
				// Permanently disable until the user removes it or resets the circuit.
				cb.openForPoW()
				slog.Warn("relay requires proof-of-work (NIP-13); disabling until manually reset — consider removing this relay",
					"relay", result.RelayURL, "error", result.Error)
			} else if isPolicyRejection(result.Error) {
				// Relay is healthy but rejected the event content via NIP-01.
				// Record success to keep circuit closed (preventing IP bans is not needed).
				cb.recordSuccess()
				slog.Debug("relay rejected event by policy", "relay", result.RelayURL, "id", event.ID, "error", result.Error)
				failed++ // Count as publish failure for this specific event
			} else {
				justOpened := cb.recordFailure()
				if justOpened {
					slog.Warn("relay circuit opened; will retry in 5 minutes",
						"relay", result.RelayURL, "error", result.Error)
				} else if st := cb.status(result.RelayURL); !st.CircuitOpen {
					// Below threshold: log the individual failure.
					slog.Warn("failed to publish event",
						"relay", result.RelayURL, "id", event.ID, "error", result.Error,
						"fail_count", st.FailCount)
				}
				// Circuit already open (not just opened): suppress log to avoid spam.
			}
			failed++
		} else {
			wasOpen := cb.recordSuccess()
			if wasOpen {
				slog.Info("relay recovered", "relay", result.RelayURL)
			}
			slog.Debug("published event", "relay", result.RelayURL, "id", event.ID, "kind", event.Kind)
			published++
		}
	}

	if published == 0 && failed > 0 {
		return fmt.Errorf("failed to publish to all %d active relays", failed)
	}
	return nil
}

// isPowRequired returns true if the relay rejected the event due to a
// proof-of-work requirement (NIP-13). The relay error message contains "pow:".
func isPowRequired(err error) bool {
	return err != nil && strings.Contains(err.Error(), "pow:")
}

// isPolicyRejection returns true if the relay rejected the event with a NIP-01
// machine-readable prefix indicating a static policy refusal.
func isPolicyRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "msg: blocked:") || strings.Contains(msg, "msg: invalid:")
}
