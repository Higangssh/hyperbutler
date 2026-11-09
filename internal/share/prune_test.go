package share

import (
	"fmt"
	"testing"
	"time"
)

// TestRecentAndChangedPruned: names older than their window are forgotten on the next
// mark once the window has passed, not only past 65536 or maxChanged entries (the 24 h
// soak showed the sets growing with ever new names).
func TestRecentAndChangedPruned(t *testing.T) {
	s := NewServer(Options{})
	old := time.Now().Add(-time.Minute)
	for i := 0; i < 5000; i++ {
		s.recent[nodeKey{rootNode, fmt.Sprint("old", i)}] = old
		s.changed[nodeKey{rootNode, fmt.Sprint("old", i)}] = change{seq: uint64(i + 1), at: old}
	}
	s.hostSeq = 5000

	s.markRecentLocked(rootNode, "new")
	if len(s.recent) != 1 {
		t.Errorf("recent has %d names after the window, want 1", len(s.recent))
	}
	s.noteChangeLocked(nodeKey{rootNode, "new"})
	if len(s.changed) != 1 {
		t.Errorf("changed has %d names after the window, want 1", len(s.changed))
	}
	// A forgotten change still counts as raced for a read taken before it.
	if !s.racedLocked(nodeKey{rootNode, "old7"}, 3) {
		t.Error("a read before a forgotten change is not raced")
	}

	// Within the window nothing is pruned again: a second mark keeps both names.
	s.markRecentLocked(rootNode, "new2")
	if len(s.recent) != 2 {
		t.Errorf("recent has %d names, want 2", len(s.recent))
	}
	if !s.echoLocked(rootNode, "new") || !s.echoLocked(rootNode, "new2") {
		t.Error("a fresh name is not suppressed as an echo")
	}
}

// TestNegativePruned: "missing" answers older than twice NegTTL are forgotten on the
// next one, including those whose parent node is gone (the 24 h soak of v0.2.0-rc.1
// showed the set growing with every bind mount), and fresh ones are kept.
func TestNegativePruned(t *testing.T) {
	s := NewServer(Options{NegTTL: time.Second})
	now := time.Now()
	for i := 0; i < 5000; i++ {
		s.negative[nodeKey{uint64(1000 + i), "gone"}] = now.Add(-time.Minute)
	}
	s.negative[nodeKey{rootNode, "fresh"}] = now.Add(-time.Second)

	s.pruneNegativeLocked(now)
	if len(s.negative) != 1 {
		t.Fatalf("negative has %d names after the window, want 1", len(s.negative))
	}
	if _, ok := s.negative[nodeKey{rootNode, "fresh"}]; !ok {
		t.Error("an answer the guest may still cache was forgotten")
	}

	// Within the window nothing is scanned again.
	s.negative[nodeKey{rootNode, "old"}] = now.Add(-time.Minute)
	s.pruneNegativeLocked(now.Add(time.Second))
	if len(s.negative) != 2 {
		t.Errorf("negative has %d names, want 2 (pruned again inside the window)", len(s.negative))
	}
	s.pruneNegativeLocked(now.Add(3 * time.Second))
	if len(s.negative) != 0 {
		t.Errorf("negative has %d names, want 0", len(s.negative))
	}
}
