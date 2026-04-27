package outbox

// EnqueueAdapter implements the enqueuer interfaces expected by the AP
// Federator, Nostr Publisher, and Bluesky Poster. It wraps a Queue and
// delegates to Enqueue with the appropriate dest_type.
//
// When Notifier is set, the matching worker is woken immediately after a
// successful enqueue so it can drain the queue without waiting for the
// fallback poll tick.
type EnqueueAdapter struct {
	Queue    *Queue
	Notifier Notifier
}

func (e *EnqueueAdapter) wake(destType string) {
	if e.Notifier != nil {
		e.Notifier.Notify(destType)
	}
}

// EnqueueAP satisfies ap.Enqueuer.
func (e *EnqueueAdapter) EnqueueAP(destURL, payload string, priority int, sourceEventID string) error {
	_, err := e.Queue.Enqueue(Item{
		DestType:      "ap",
		DestURL:       destURL,
		Payload:       payload,
		Priority:      priority,
		SourceEventID: sourceEventID,
	})
	if err == nil {
		e.wake("ap")
	}
	return err
}

// EnqueueRelay satisfies nostr.PublishEnqueuer.
func (e *EnqueueAdapter) EnqueueRelay(destURL, payload string, priority int, sourceEventID string) error {
	_, err := e.Queue.Enqueue(Item{
		DestType:      "relay",
		DestURL:       destURL,
		Payload:       payload,
		Priority:      priority,
		SourceEventID: sourceEventID,
	})
	if err == nil {
		e.wake("relay")
	}
	return err
}

// EnqueueBsky satisfies bsky.PosterEnqueuer.
func (e *EnqueueAdapter) EnqueueBsky(destURL, payload string, priority int, sourceEventID string) error {
	_, err := e.Queue.Enqueue(Item{
		DestType:      "bsky",
		DestURL:       destURL,
		Payload:       payload,
		Priority:      priority,
		SourceEventID: sourceEventID,
	})
	if err == nil {
		e.wake("bsky")
	}
	return err
}
