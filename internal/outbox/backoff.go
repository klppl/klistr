package outbox

import "time"

// defaultSchedule defines the delay before each retry attempt.
// Index 0 = attempt 1 (immediate), index 5 = attempt 6 (1 hour).
var defaultSchedule = []time.Duration{
	0,                  // attempt 1: immediate
	5 * time.Second,    // attempt 2
	30 * time.Second,   // attempt 3
	2 * time.Minute,    // attempt 4
	10 * time.Minute,   // attempt 5
	1 * time.Hour,      // attempt 6
}

// DefaultBackoff returns the delay before the given attempt number (1-based).
// Attempts beyond the schedule length return the last entry.
func DefaultBackoff(attempt int) time.Duration {
	return backoffFor(defaultSchedule, attempt)
}

func backoffFor(schedule []time.Duration, attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	idx := attempt - 1
	if idx >= len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[idx]
}
