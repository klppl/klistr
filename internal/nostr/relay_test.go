package nostr

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

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
