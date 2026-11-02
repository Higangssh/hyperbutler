package engine

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// A host listener that sleeps after a failed accept must still send its secret before
// the guest gives up on it (review of the two-account fix): the cap stays well below
// the guest's timeout, and the backoff never passes it.
func TestAcceptBackoffBelowSecretTimeout(t *testing.T) {
	if 4*maxAcceptBackoff > vmsecret.DefaultTimeout {
		t.Fatalf("maxAcceptBackoff %v is not well below vmsecret.DefaultTimeout %v", maxAcceptBackoff, vmsecret.DefaultTimeout)
	}
	var b time.Duration
	for range 20 {
		b = nextAcceptBackoff(b)
		if b <= 0 || b > maxAcceptBackoff {
			t.Fatalf("backoff %v", b)
		}
	}
	if b != maxAcceptBackoff {
		t.Fatalf("backoff settles at %v", b)
	}
}

// logRecord is one record a recordHandler kept.
type logRecord struct {
	level slog.Level
	msg   string
	hint  string
}

// recordHandler keeps every record, for tests that check what was logged.
type recordHandler struct {
	mu   sync.Mutex
	recs []logRecord
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordHandler) WithGroup(string) slog.Handler            { return h }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "hint" {
			rec.hint = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	h.recs = append(h.recs, rec)
	h.mu.Unlock()
	return nil
}

// waitSilent waits until h holds n records whose message contains msg, and checks
// that the first is a warning with the hint and the others are debug.
func (h *recordHandler) waitSilent(t *testing.T, msg string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		var got []logRecord
		for _, r := range h.recs {
			if strings.Contains(r.msg, msg) {
				got = append(got, r)
			}
		}
		h.mu.Unlock()
		if len(got) >= n {
			for i, r := range got {
				want := slog.LevelDebug
				if i == 0 {
					want = slog.LevelWarn
				}
				if r.level != want || !strings.Contains(r.hint, "VM secret") {
					t.Fatalf("record %d: %+v, want level %v with the hint", i, r, want)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d %q records logged: %+v", len(got), n, msg, h.recs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
