package server

import (
	"io"
	"strings"
	"sync"
)

const logBufSize = 500

// LogBroadcaster is an io.Writer that captures every log line written to it,
// maintains a ring buffer of recent lines, and fans them out to SSE subscribers.
type LogBroadcaster struct {
	out io.Writer // underlying writer (e.g. os.Stdout)

	mu   sync.Mutex
	buf  []string
	subs []chan string
}

// NewLogBroadcaster returns a LogBroadcaster that also writes every byte to out.
func NewLogBroadcaster(out io.Writer) *LogBroadcaster {
	return &LogBroadcaster{
		out: out,
		buf: make([]string, 0, logBufSize),
	}
}

// Write implements io.Writer. Every call is expected to be one JSON log line.
func (lb *LogBroadcaster) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")

	lb.mu.Lock()
	lb.buf = append(lb.buf, line)
	if len(lb.buf) > logBufSize {
		lb.buf = lb.buf[len(lb.buf)-logBufSize:]
	}
	for _, ch := range lb.subs {
		select {
		case ch <- line:
		default: // slow consumer: drop rather than block
		}
	}
	lb.mu.Unlock()

	return lb.out.Write(p)
}

// Lines returns a snapshot of the current ring buffer. No subscription is created.
func (lb *LogBroadcaster) Lines() []string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	out := make([]string, len(lb.buf))
	copy(out, lb.buf)
	return out
}

// Subscribe returns a snapshot of recent log lines, a channel for new lines,
// and a cancel func that must be called when the subscriber is done.
func (lb *LogBroadcaster) Subscribe() (history []string, ch <-chan string, cancel func()) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	history = make([]string, len(lb.buf))
	copy(history, lb.buf)

	c := make(chan string, 128)
	lb.subs = append(lb.subs, c)

	cancel = func() {
		lb.mu.Lock()
		defer lb.mu.Unlock()
		for i, s := range lb.subs {
			if s == c {
				lb.subs = append(lb.subs[:i], lb.subs[i+1:]...)
				break
			}
		}
		close(c)
	}
	return history, c, cancel
}
