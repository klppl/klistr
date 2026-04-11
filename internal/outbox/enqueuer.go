package outbox

// EnqueueAdapter implements the enqueuer interfaces expected by the AP
// Federator, Nostr Publisher, and Bluesky Poster. It wraps a Queue and
// delegates to Enqueue with the appropriate dest_type.
type EnqueueAdapter struct {
	Queue *Queue
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
	return err
}
