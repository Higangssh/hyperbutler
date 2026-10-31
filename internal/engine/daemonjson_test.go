package engine

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
)

// fakeGuest accepts any daemon.json that does not contain "dockerd-refuses" and records
// what it got.
type fakeGuest struct {
	features guestctl.Features
	got      [][]byte
	restarts []bool
	rollback bool // the next restart rolls back
	err      error
}

func (g *fakeGuest) Hello() guestctl.HelloReply { return guestctl.HelloReply{Features: g.features} }

func (g *fakeGuest) DaemonConfig(_ context.Context, data []byte, restart bool) (guestctl.DaemonResult, error) {
	if g.err != nil {
		return guestctl.DaemonResult{}, g.err
	}
	g.got = append(g.got, bytes.Clone(data))
	g.restarts = append(g.restarts, restart)
	if !restart && bytes.Contains(data, []byte("dockerd-exits")) {
		// At boot: dockerd exits with the file, so the guest starts it without one.
		return guestctl.DaemonResult{Code: guestctl.DaemonRolledBack, Flags: guestctl.DaemonChanged | guestctl.DaemonRestarted, Message: "dockerd did not start"}, nil
	}
	if bytes.Contains(data, []byte("dockerd-refuses")) {
		return guestctl.DaemonResult{Code: guestctl.DaemonRejected, Message: "unknown key \x1b[31m"}, nil
	}
	if restart && g.rollback {
		g.rollback = false
		return guestctl.DaemonResult{Code: guestctl.DaemonRolledBack, Flags: guestctl.DaemonChanged | guestctl.DaemonRestarted, Message: "dockerd did not start"}, nil
	}
	r := guestctl.DaemonResult{Flags: guestctl.DaemonChanged}
	if restart {
		r.Flags |= guestctl.DaemonRestarted
	}
	return r, nil
}

func newDaemonTest(t *testing.T) (*DaemonConfig, *fakeGuest, *[]DaemonState) {
	l := Layout{Home: testDir(t)}
	l.Data = l.Home
	var states []DaemonState
	dc := &DaemonConfig{Layout: l, GuestSubnet: netip.MustParsePrefix("192.168.127.0/24"), OnState: func(s DaemonState) {
		if err := s.Validate(); err != nil {
			t.Errorf("invalid state %+v: %v", s, err)
		}
		states = append(states, s)
	}}
	return dc, &fakeGuest{features: guestctl.FeatureDaemonConfig}, &states
}

func writeUser(t *testing.T, dc *DaemonConfig, s string) {
	t.Helper()
	if err := os.WriteFile(dc.Layout.DaemonConfigPath(), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonConfigBoot(t *testing.T) {
	ctx := context.Background()
	t.Run("none", func(t *testing.T) {
		dc, g, _ := newDaemonTest(t)
		dc.Connected(ctx, g)
		if len(g.got) != 1 || g.got[0] != nil || g.restarts[0] {
			t.Fatalf("pushed %q %v", g.got, g.restarts)
		}
		if st := dc.State(); st.Source != DaemonNone || st.Error != "" || st.Generation != 1 {
			t.Fatalf("state %+v", st)
		}
	})
	t.Run("user file, byte for byte", func(t *testing.T) {
		dc, g, _ := newDaemonTest(t)
		user := "{\r\n  \"insecure-registries\" : [\"192.168.1.5:5000\"]\r\n}\r\n"
		writeUser(t, dc, user)
		dc.Connected(ctx, g)
		if len(g.got) != 1 || string(g.got[0]) != user {
			t.Fatalf("pushed %q", g.got)
		}
		st := dc.State()
		if st.Source != DaemonFile || st.Bytes != len(user) || st.SHA256 != digest([]byte(user)) {
			t.Fatalf("state %+v", st)
		}
		if b, _ := os.ReadFile(dc.Layout.DaemonAppliedPath()); string(b) != user {
			t.Fatalf("applied copy %q", b)
		}
		// A reconnect sends what is in effect, even if the user file changed meanwhile.
		writeUser(t, dc, `{"debug":true}`)
		dc.Connected(ctx, g)
		if len(g.got) != 2 || string(g.got[1]) != user || g.restarts[1] {
			t.Fatalf("reconnect pushed %q", g.got)
		}
	})
	t.Run("invalid user file keeps the previous one", func(t *testing.T) {
		dc, g, _ := newDaemonTest(t)
		prev := `{"debug":true}`
		if err := WriteFileAtomic(dc.Layout.DaemonAppliedPath(), []byte(prev)); err != nil {
			t.Fatal(err)
		}
		writeUser(t, dc, `{"hosts":["tcp://0.0.0.0:2375"]}`)
		dc.Connected(ctx, g)
		if len(g.got) != 1 || string(g.got[0]) != prev {
			t.Fatalf("pushed %q", g.got)
		}
		st := dc.State()
		if st.Source != DaemonPrevious || !strings.Contains(st.Error, "hyperbutler owns") || !strings.Contains(st.Error, "previous") {
			t.Fatalf("state %+v", st)
		}
		// The user's file is untouched.
		if b, _ := os.ReadFile(dc.Layout.DaemonConfigPath()); string(b) != `{"hosts":["tcp://0.0.0.0:2375"]}` {
			t.Fatalf("user file rewritten: %q", b)
		}
		if b, _ := os.ReadFile(dc.Layout.DaemonAppliedPath()); string(b) != prev {
			t.Fatalf("applied copy changed: %q", b)
		}
	})
	t.Run("dockerd refuses the user file", func(t *testing.T) {
		dc, g, _ := newDaemonTest(t)
		writeUser(t, dc, `{"dockerd-refuses":1}`)
		dc.Connected(ctx, g)
		if len(g.got) != 2 || g.got[1] != nil {
			t.Fatalf("pushed %q", g.got)
		}
		st := dc.State()
		if st.Source != DaemonNone || !strings.Contains(st.Error, "unknown key") || strings.Contains(st.Error, "\x1b") {
			t.Fatalf("state %+v", st)
		}
	})
	t.Run("dockerd exits at boot", func(t *testing.T) {
		dc, g, _ := newDaemonTest(t)
		prev := `{"debug":true}`
		if err := WriteFileAtomic(dc.Layout.DaemonAppliedPath(), []byte(prev)); err != nil {
			t.Fatal(err)
		}
		writeUser(t, dc, `{"dockerd-exits":1}`)
		dc.Connected(ctx, g)
		// The user's file, then the previous one with a dockerd restart.
		if len(g.got) != 2 || string(g.got[1]) != prev || g.restarts[0] || !g.restarts[1] {
			t.Fatalf("pushed %q %v", g.got, g.restarts)
		}
		if st := dc.State(); st.Source != DaemonPrevious || !strings.Contains(st.Error, "did not start") {
			t.Fatalf("state %+v", st)
		}
		// Without a previous one, dockerd stays without a file.
		dc2, g2, _ := newDaemonTest(t)
		writeUser(t, dc2, `{"dockerd-exits":1}`)
		dc2.Connected(ctx, g2)
		if len(g2.got) != 1 {
			t.Fatalf("pushed %q", g2.got)
		}
		if st := dc2.State(); st.Source != DaemonNone || st.Error == "" {
			t.Fatalf("state %+v", st)
		}
	})
	t.Run("old guest", func(t *testing.T) {
		dc, g, _ := newDaemonTest(t)
		g.features = 0
		writeUser(t, dc, `{"debug":true}`)
		dc.Connected(ctx, g)
		if len(g.got) != 0 || !strings.Contains(dc.State().Error, "does not support") {
			t.Fatalf("pushed %q, state %+v", g.got, dc.State())
		}
		if err := dc.Apply(ctx); err == nil || !strings.Contains(err.Error(), "does not support") {
			t.Fatalf("apply: %v", err)
		}
	})
}

func TestDaemonConfigApply(t *testing.T) {
	ctx := context.Background()
	dc, g, _ := newDaemonTest(t)
	if err := dc.Apply(ctx); !errors.Is(err, ErrNoGuest) {
		t.Fatalf("apply before a connection: %v", err)
	}
	dc.Connected(ctx, g)
	writeUser(t, dc, `{"insecure-registries":["10.1.2.3:5000"]}`)
	if err := dc.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	st := dc.State()
	if st.Source != DaemonFile || !st.Restarted || st.Error != "" || st.Applies != 2 || !g.restarts[len(g.restarts)-1] {
		t.Fatalf("state %+v", st)
	}
	// An owned key: nothing is sent, the previous file stays.
	n := len(g.got)
	writeUser(t, dc, `{"Data-Root":"/x"}`)
	if err := dc.Apply(ctx); err == nil || !strings.Contains(err.Error(), "hyperbutler owns") {
		t.Fatalf("owned key: %v", err)
	}
	if len(g.got) != n || dc.State().Source != DaemonFile || dc.State().SHA256 != st.SHA256 {
		t.Fatalf("an owned key was sent or changed the state: %+v", dc.State())
	}
	// Invalid JSON: the same.
	writeUser(t, dc, `{"debug": tru`)
	if err := dc.Apply(ctx); err == nil || !strings.Contains(err.Error(), "previous daemon.json stays") {
		t.Fatalf("bad JSON: %v", err)
	}
	// dockerd refuses it.
	writeUser(t, dc, `{"dockerd-refuses":true}`)
	if err := dc.Apply(ctx); err == nil || dc.State().SHA256 != st.SHA256 {
		t.Fatalf("refused: %v %+v", err, dc.State())
	}
	// dockerd does not come up with it: the guest rolled back.
	g.rollback = true
	writeUser(t, dc, `{"debug":true}`)
	if err := dc.Apply(ctx); err == nil || !strings.Contains(err.Error(), "did not start") || dc.State().SHA256 != st.SHA256 {
		t.Fatalf("rollback: %v %+v", err, dc.State())
	}
	if b, _ := os.ReadFile(dc.Layout.DaemonAppliedPath()); string(b) != `{"insecure-registries":["10.1.2.3:5000"]}` {
		t.Fatalf("applied copy %q", b)
	}
	// Removing the file applies "none" and forgets the copy.
	if err := os.Remove(dc.Layout.DaemonConfigPath()); err != nil {
		t.Fatal(err)
	}
	if err := dc.Apply(ctx); err != nil || dc.State().Source != DaemonNone {
		t.Fatalf("removal: %v %+v", err, dc.State())
	}
	if _, err := os.Stat(dc.Layout.DaemonAppliedPath()); !os.IsNotExist(err) {
		t.Fatal("applied copy kept")
	}
	// A transport error is reported, the state kept.
	g.err = errors.New("connection closed")
	writeUser(t, dc, `{}`)
	if err := dc.Apply(ctx); err == nil || dc.State().Source != DaemonNone {
		t.Fatalf("transport error: %v", err)
	}
}

func TestGuestText(t *testing.T) {
	if got := guestText("a\x1b[2Jb\u202ec\u200bd\ufeff"); got != `a\u001b[2Jb\u202ec\u200bd\ufeff` {
		t.Fatalf("%q", got)
	}
}

func TestDaemonStateValidate(t *testing.T) {
	ok := DaemonState{Source: DaemonFile, SHA256: digest([]byte("{}")), Bytes: 2}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*DaemonState){
		"source":       func(d *DaemonState) { d.Source = "x" },
		"none w/ hash": func(d *DaemonState) { d.Source = DaemonNone },
		"hash case":    func(d *DaemonState) { d.SHA256 = strings.ToUpper(d.SHA256) },
		"hash short":   func(d *DaemonState) { d.SHA256 = d.SHA256[:62] },
		"no bytes":     func(d *DaemonState) { d.Bytes = 0 },
		"bytes":        func(d *DaemonState) { d.Bytes = 1 << 20 },
		"error utf8":   func(d *DaemonState) { d.Error = "\xff" },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, c)
		}
	}
}
