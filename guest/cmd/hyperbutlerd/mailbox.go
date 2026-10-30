package main

import (
	"sync"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

// mailbox carries budget and connection-lost events from the control connections to the
// guard loop. Only the current connection may change the guard's state: events from an
// older connection (one that was replaced by a reconnect) are dropped when they are
// posted, so a late ConnLost from a dead connection can neither fail the guard closed
// nor evict the new connection's budget.
type mailbox struct {
	mu     sync.Mutex
	gen    uint64 // the current connection
	budget *spaceguard.Budget
	lost   bool
	notify chan struct{}
}

func newMailbox() *mailbox { return &mailbox{notify: make(chan struct{}, 1)} }

// connect starts a new connection generation and returns it.
func (m *mailbox) connect() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen++
	return m.gen
}

// postBudget records a budget from connection gen.
func (m *mailbox) postBudget(gen uint64, b spaceguard.Budget) {
	m.post(gen, func() { m.budget, m.lost = &b, false })
}

// postLost records that connection gen is gone.
func (m *mailbox) postLost(gen uint64) {
	m.post(gen, func() { m.budget, m.lost = nil, true })
}

func (m *mailbox) post(gen uint64, apply func()) {
	m.mu.Lock()
	if gen != m.gen {
		m.mu.Unlock()
		return
	}
	apply()
	m.mu.Unlock()
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

// take returns the pending events: lost first, then the newest budget. Either may be
// absent. A budget posted after a lost replaces it, since that connection is live again.
func (m *mailbox) take() (lost bool, b *spaceguard.Budget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lost, b = m.lost, m.budget
	m.lost, m.budget = false, nil
	return lost, b
}
