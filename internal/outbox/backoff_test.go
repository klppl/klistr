package outbox

import (
	"testing"
	"time"
)

func TestDefaultBackoff(t *testing.T) {
	tests := []struct {
		name     string
		attempt  int
		expected time.Duration
	}{
		{"attempt 1 is immediate", 1, 0},
		{"attempt 2 is 5s", 2, 5 * time.Second},
		{"attempt 3 is 30s", 3, 30 * time.Second},
		{"attempt 4 is 2m", 4, 2 * time.Minute},
		{"attempt 5 is 10m", 5, 10 * time.Minute},
		{"attempt 6 is 1h", 6, 1 * time.Hour},
		{"attempt 0 clamps to immediate", 0, 0},
		{"attempt 99 clamps to 1h", 99, 1 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DefaultBackoff(tc.attempt)
			if got != tc.expected {
				t.Errorf("DefaultBackoff(%d) = %v, want %v", tc.attempt, got, tc.expected)
			}
		})
	}
}
