// Package session is the engine's side of one sandbox session's project (ADR 0009):
// it copies the project in through the session's guest agent and builds the review of
// what the agent changed, holding the reviewed bytes so that apply writes exactly what
// the user saw and never reads the guest again.
package session

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/Higangssh/hyperbutler/internal/sandbox/apply"
	"github.com/Higangssh/hyperbutler/internal/sandbox/copyin"
	"github.com/Higangssh/hyperbutler/internal/sandbox/review"
	"github.com/Higangssh/hyperbutler/internal/sandbox/wire"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// Review limits.
const (
	DefaultMaxFile  = 64 << 20
	DefaultMaxTotal = 1 << 30
)

// Session reaches one session's guest agent.
type Session struct {
	// Copy dials the agent's copy port, Changes its list and read port; both send the
	// session's VM secret first.
	Copy, Changes transport.Dialer
	// MaxFile and MaxTotal bound what a review reads from the guest.
	MaxFile, MaxTotal int64
}

// CopyIn sends plan's files from dir and waits for the agent to say they are on its
// disk. It returns what was sent, the base of the conflict check at apply.
func (s *Session) CopyIn(ctx context.Context, dir string, plan copyin.Plan) (map[string]copyin.Copied, error) {
	c, err := s.Copy.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("session copy port: %w", err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	sent, err := copyin.WriteTar(c, dir, plan)
	if err != nil {
		return nil, err
	}
	var ack [1]byte
	if _, err := io.ReadFull(c, ack[:]); err != nil || ack[0] != 1 {
		return nil, fmt.Errorf("the session did not confirm the copy: %w", errors.Join(err, ctx.Err()))
	}
	return sent, nil
}

// Item is one change in a review. Everything from the guest in it is shown through
// review.Escape or Shown.
type Item struct {
	Path string
	Kind wire.Kind
	// Shown is the path as the review shows it.
	Shown string
	// Risk says why it is listed first; "" for an ordinary edit.
	Risk review.Risk
	// Refused says why it cannot be applied (a path outside the grammar, a link, a file
	// over the limit); such an item is shown but never applied.
	Refused string
	// Change is what apply writes: the bytes read once, here.
	Change apply.Change
	// Sum is the SHA-256 of Change.Data, computed on the host.
	Sum [sha256.Size]byte
}

// Review lists the session's changes and reads each changed file once. sent is what
// CopyIn returned. Items are sorted with the ones that carry a risk first.
func (s *Session) Review(ctx context.Context, sent map[string]copyin.Copied) ([]Item, error) {
	maxFile, maxTotal := s.MaxFile, s.MaxTotal
	if maxFile == 0 {
		maxFile = DefaultMaxFile
	}
	if maxTotal == 0 {
		maxTotal = DefaultMaxTotal
	}
	c, err := s.Changes.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("session changes port: %w", err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	wc := wire.NewClient(c)
	list, err := wc.List()
	if err != nil {
		return nil, err
	}
	var items []Item
	var total int64
	for _, e := range list {
		it := Item{Path: e.Path, Kind: e.Kind, Shown: review.Shorten(e.Path, 200)}
		it.Change = apply.Change{Path: e.Path, Delete: e.Kind == wire.Deleted}
		if old, ok := sent[e.Path]; ok {
			sum := old.Sum
			it.Change.Base = &sum
		}
		_, perr := review.CheckPath(e.Path)
		it.Risk = review.Classify(e.Path, e.Kind == wire.Added && e.Flags&wire.FlagExec != 0)
		switch {
		case perr != nil:
			it.Refused = perr.Error()
		case it.Risk == review.RiskGitInner:
			it.Refused = "inside .git; commits come out as a bundle"
		case e.Flags&wire.FlagLink != 0:
			it.Refused = "a symbolic link (to " + review.Shorten(e.Link, 120) + "); links are not applied"
		case it.Change.Delete:
		case e.Size > uint64(maxFile): //nolint:gosec // G115: maxFile is positive
			it.Refused = fmt.Sprintf("larger than %d bytes", maxFile)
		default:
			data, err := wc.ReadFile(e.Path, min(maxFile, maxTotal-total))
			if err != nil {
				var ge *wire.GuestError
				if errors.As(err, &ge) || errors.Is(err, wire.ErrTooLarge) {
					it.Refused = "unreadable in the session: " + err.Error()
					break
				}
				return nil, err
			}
			total += int64(len(data))
			it.Change.Data, it.Sum = data, sha256.Sum256(data)
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool { return (items[i].Risk != "") && (items[j].Risk == "") })
	return items, nil
}

// Accepted returns the changes of the items the user accepted, by path. An item that
// was refused is never returned.
func Accepted(items []Item, accept func(Item) bool) []apply.Change {
	var out []apply.Change
	for _, it := range items {
		if it.Refused == "" && accept(it) {
			out = append(out, it.Change)
		}
	}
	return out
}
