package main

import (
	"testing"

	"github.com/Higangssh/winbox/internal/spaceguard"
)

// The review's M1 sequence: connection 2 posts a budget, then connection 1's deferred
// ConnLost arrives. The budget must survive and no lost event may be seen.
func TestMailboxStaleLostDoesNotEvictBudget(t *testing.T) {
	m := newMailbox()
	g1 := m.connect()
	m.postBudget(g1, spaceguard.Budget{Bytes: 1})
	m.take()
	g2 := m.connect()
	m.postBudget(g2, spaceguard.Budget{Bytes: 2})
	m.postLost(g1)
	lost, b := m.take()
	if lost || b == nil || b.Bytes != 2 {
		t.Fatalf("lost=%v budget=%v", lost, b)
	}
}

func TestMailboxCurrentLost(t *testing.T) {
	m := newMailbox()
	g := m.connect()
	m.postBudget(g, spaceguard.Budget{Bytes: 1})
	m.postLost(g)
	lost, b := m.take()
	if !lost || b != nil {
		t.Fatalf("lost=%v budget=%v", lost, b)
	}
	select {
	case <-m.notify:
	default:
		t.Fatal("no notification")
	}
}

func TestMailboxStaleBudgetDropped(t *testing.T) {
	m := newMailbox()
	g1 := m.connect()
	m.connect()
	m.postBudget(g1, spaceguard.Budget{Bytes: 9})
	if lost, b := m.take(); lost || b != nil {
		t.Fatalf("stale budget delivered: %v %v", lost, b)
	}
}
