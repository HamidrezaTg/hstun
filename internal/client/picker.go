package client

import (
	"log/slog"
	"sync"
	"time"
)

// Failover timing. Variables, so tests can shorten them.
var (
	autoFailAfter  = 3                // consecutive failed attempts on the current carrier before moving to the next
	autoProbeEvery = 10 * time.Minute // how often a better carrier than the current one is tried again
)

// defaultAutoOrder is the order tried by carrier = "auto": the fast WebSocket
// first, then the long-poll http carrier, then one-connection-per-request chunk.
var defaultAutoOrder = []string{"ws", "http", "chunk"}

// picker holds which carrier the slots use. With one carrier it never changes.
// With "auto" it moves down the list after autoFailAfter consecutive failed
// attempts and moves back up when a probe of a better carrier succeeds.
type picker struct {
	log   *slog.Logger
	order []string

	mu      sync.Mutex
	cur     int
	fails   int
	changed chan struct{} // closed and replaced on every switch, so sessions on the old carrier can end
}

func newPicker(order []string, log *slog.Logger) *picker {
	return &picker{log: log, order: order, changed: make(chan struct{})}
}

// current is the carrier a new session should use, and a channel that is
// closed when the choice changes.
func (p *picker) current() (string, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.order[p.cur], p.changed
}

func (p *picker) auto() bool { return len(p.order) > 1 }

func (p *picker) switchLocked(to int, why string) {
	p.log.Warn("carrier switched", "from", p.order[p.cur], "to", p.order[to], "why", why)
	p.cur, p.fails = to, 0
	close(p.changed)
	p.changed = make(chan struct{})
}

// report records the result of one attempt on a carrier: up is true when a
// session got established. It returns true when this call switched carrier.
func (p *picker) report(carrier string, up bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if carrier != p.order[p.cur] || len(p.order) < 2 {
		return false // a late result for a carrier we already left
	}
	if up {
		p.fails = 0
		return false
	}
	p.fails++
	if p.fails < autoFailAfter {
		return false
	}
	p.switchLocked((p.cur+1)%len(p.order), "no session after repeated attempts")
	return true
}

// better lists the carriers ranked above the current one, best first.
func (p *picker) better() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.order[:p.cur]...)
}

// promote moves to a carrier ranked above the current one, after a probe of it worked.
func (p *picker) promote(carrier string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < p.cur; i++ {
		if p.order[i] == carrier {
			p.switchLocked(i, "a better carrier works again")
			return
		}
	}
}

// SetAutoTiming changes the failover timing and returns a function that
// restores it. It exists for tests in other packages.
func SetAutoTiming(failAfter int, probeEvery time.Duration) (restore func()) {
	oldF, oldP := autoFailAfter, autoProbeEvery
	autoFailAfter, autoProbeEvery = failAfter, probeEvery
	return func() { autoFailAfter, autoProbeEvery = oldF, oldP }
}
