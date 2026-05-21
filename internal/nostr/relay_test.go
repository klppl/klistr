package nostr

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

type mockEnqueuer struct {
	enqueued []struct {
		url      string
		kind     int
		priority int
	}
}

func (m *mockEnqueuer) EnqueueRelay(url, payload string, priority int, sourceEventID string) error {
	var event nostr.Event
	_ = json.Unmarshal([]byte(payload), &event)
	m.enqueued = append(m.enqueued, struct {
		url      string
		kind     int
		priority int
	}{url, event.Kind, priority})
	return nil
}

func TestPublisher_KindFiltering(t *testing.T) {
	relays := []string{"wss://relay1.com", "wss://relay2.com"}
	p := NewPublisher(relays)
	enqueuer := &mockEnqueuer{}
	p.Enqueuer = enqueuer

	t.Run("Learn restriction from error", func(t *testing.T) {
		url := "wss://relay1.com"
		err := fmt.Errorf("msg: blocked: kind 1 is not allowed")
		p.RecordRelayResult(url, err)

		if p.IsKindAllowed(url, 1) {
			t.Error("expected kind 1 to be restricted for relay1")
		}
		if !p.IsKindAllowed(url, 0) {
			t.Error("expected kind 0 to be allowed for relay1")
		}
		if !p.IsKindAllowed("wss://relay2.com", 1) {
			t.Error("expected kind 1 to be allowed for relay2")
		}
	})

	t.Run("Publish skips restricted relays", func(t *testing.T) {
		enqueuer.enqueued = nil
		event := &nostr.Event{
			Kind:    1,
			Content: "hello",
		}
		_ = p.Publish(context.Background(), event)

		for _, e := range enqueuer.enqueued {
			if e.url == "wss://relay1.com" {
				t.Error("expected kind 1 to be skipped for relay1")
			}
		}

		foundRelay2 := false
		for _, e := range enqueuer.enqueued {
			if e.url == "wss://relay2.com" {
				foundRelay2 = true
			}
		}
		if !foundRelay2 {
			t.Error("expected kind 1 to be enqueued for relay2")
		}
	})

	t.Run("Allowed kinds are still sent", func(t *testing.T) {
		enqueuer.enqueued = nil
		event := &nostr.Event{
			Kind:    0,
			Content: "{}",
		}
		_ = p.Publish(context.Background(), event)

		foundRelay1 := false
		foundRelay2 := false
		for _, e := range enqueuer.enqueued {
			if e.url == "wss://relay1.com" {
				foundRelay1 = true
			}
			if e.url == "wss://relay2.com" {
				foundRelay2 = true
			}
		}
		if !foundRelay1 || !foundRelay2 {
			t.Error("expected kind 0 to be enqueued for both relays")
		}
	})
}

// TestRelayCircuit_AutoQuarantine verifies that a relay failing continuously for
// longer than quarantineAfter is auto-disabled (quarantined) rather than cycling
// open/half-open forever.
func TestRelayCircuit_AutoQuarantine(t *testing.T) {
	// Shorten the threshold for the test and restore it afterwards.
	orig := quarantineAfter
	quarantineAfter = 50 * time.Millisecond
	defer func() { quarantineAfter = orig }()

	cb := &relayCircuit{}

	// First failures (before the threshold elapses) open the circuit normally
	// but do not quarantine.
	for i := 0; i < cbThreshold; i++ {
		opened, quarantined := cb.recordFailure()
		if quarantined {
			t.Fatalf("quarantined too early on failure %d", i+1)
		}
		_ = opened
	}
	if cb.isDisabled() {
		t.Fatal("relay disabled before quarantineAfter elapsed")
	}

	// After the threshold elapses, the next failure quarantines the relay.
	time.Sleep(quarantineAfter + 10*time.Millisecond)
	_, quarantined := cb.recordFailure()
	if !quarantined {
		t.Fatal("expected quarantine after sustained failure")
	}
	if !cb.isDisabled() {
		t.Fatal("expected relay to be disabled after quarantine")
	}
	if !cb.isOpen() {
		t.Fatal("expected circuit open after quarantine")
	}
	if st := cb.status("wss://x"); !st.Quarantined || !st.CircuitOpen {
		t.Fatalf("status should report quarantined+open, got %+v", st)
	}

	// A manual reset re-enables the relay.
	cb.reset()
	if cb.isDisabled() || cb.isOpen() {
		t.Fatal("expected reset to clear quarantine")
	}
}

// TestRelayCircuit_SuccessClearsStreak verifies a success resets the
// unreachable streak so an intermittent relay is never quarantined.
func TestRelayCircuit_SuccessClearsStreak(t *testing.T) {
	orig := quarantineAfter
	quarantineAfter = 50 * time.Millisecond
	defer func() { quarantineAfter = orig }()

	cb := &relayCircuit{}
	cb.recordFailure()
	time.Sleep(30 * time.Millisecond)
	cb.recordSuccess() // streak reset here
	time.Sleep(30 * time.Millisecond)
	// Total elapsed > quarantineAfter, but the streak restarted after success.
	_, quarantined := cb.recordFailure()
	if quarantined {
		t.Fatal("a relay that recovered should not be quarantined on a fresh failure")
	}
}

// TestPublisher_SkipsDisabledRelayEnqueue verifies that a quarantined relay is
// not enqueued for, so a dead relay can't rebuild an outbox backlog.
func TestPublisher_SkipsDisabledRelayEnqueue(t *testing.T) {
	p := NewPublisher([]string{"wss://dead.com", "wss://live.com"})
	enqueuer := &mockEnqueuer{}
	p.Enqueuer = enqueuer

	// Force the dead relay into a quarantined state.
	p.getCircuit("wss://dead.com").reset()
	deadCB := p.getCircuit("wss://dead.com")
	deadCB.mu.Lock()
	deadCB.quarantined = true
	deadCB.mu.Unlock()

	_ = p.Publish(context.Background(), &nostr.Event{Kind: 1, Content: "hi"})

	for _, e := range enqueuer.enqueued {
		if e.url == "wss://dead.com" {
			t.Error("expected quarantined relay to be skipped for enqueue")
		}
	}
	foundLive := false
	for _, e := range enqueuer.enqueued {
		if e.url == "wss://live.com" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Error("expected live relay to still be enqueued")
	}
}
