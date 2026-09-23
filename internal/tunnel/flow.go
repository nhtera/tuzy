// Package tunnel implements the tuzy agent: one WebSocket session to the edge speaking wire
// protocol v1 (protocol/PROTOCOL.md), relaying each stream to a local HTTP/WebSocket target.
package tunnel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nhtera/tuzy/internal/protocol"
)

var (
	errCreditTimeout  = errors.New("credit timeout")
	errWindowOverflow = errors.New("window exceeds MaxWindow")
)

// credit is a send-credit counter (PROTOCOL.md §4.1). Waiters are woken by closing `changed`.
type credit struct {
	mu      sync.Mutex
	avail   int64
	changed chan struct{}
}

func newCredit(n int64) *credit {
	return &credit{avail: n, changed: make(chan struct{})}
}

// add grants n more bytes. Exceeding MaxWindow is a connection error.
func (c *credit) add(n int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.avail+n > protocol.MaxWindow {
		return errWindowOverflow
	}
	c.avail += n
	close(c.changed)
	c.changed = make(chan struct{})
	return nil
}

// acquire waits until both credits are positive and takes up to want bytes (bounded by both).
// It fails with errCreditTimeout after idle without any credit, or ctx.Err().
func acquire(ctx context.Context, stream, conn *credit, want int, idle time.Duration) (int, error) {
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		// Lock order: stream, then conn (the only place both are held).
		stream.mu.Lock()
		conn.mu.Lock()
		n := min(int64(want), stream.avail, conn.avail)
		if n > 0 {
			stream.avail -= n
			conn.avail -= n
		}
		sw, cw := stream.changed, conn.changed
		conn.mu.Unlock()
		stream.mu.Unlock()
		if n > 0 {
			return int(n), nil
		}
		select {
		case <-sw:
		case <-cw:
		case <-timer.C:
			return 0, errCreditTimeout
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// msgCredit counts unacked WebSocket messages we may still send (§5.2).
type msgCredit struct {
	mu          sync.Mutex
	outstanding int
	changed     chan struct{}
}

func newMsgCredit() *msgCredit { return &msgCredit{changed: make(chan struct{})} }

// take blocks until fewer than WSMsgCredit messages are unacked, then counts one more.
func (m *msgCredit) take(ctx context.Context) error {
	for {
		m.mu.Lock()
		if m.outstanding < protocol.WSMsgCredit {
			m.outstanding++
			m.mu.Unlock()
			return nil
		}
		ch := m.changed
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ack releases n messages; excess ACKs are ignored (the count never goes below 0).
func (m *msgCredit) ack(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outstanding = max(0, m.outstanding-n)
	close(m.changed)
	m.changed = make(chan struct{})
}
