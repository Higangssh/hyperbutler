//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// logNetStats logs, every interval, the stack's counters (its own and gVisor's) that
// changed, as deltas.
func logNetStats(ctx context.Context, st *netstack.Stack, every time.Duration, log *slog.Logger) {
	prev := netCounters(st)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur := netCounters(st)
		var keys []string
		for k, v := range cur {
			if v != prev[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%d", k, int64(cur[k]-prev[k])) //nolint:gosec // G115: deliberate; a gauge that went down shows as a negative delta
		}
		if b.Len() > 0 {
			log.Info("net stats" + b.String())
		}
		prev = cur
	}
}

func netCounters(st *netstack.Stack) map[string]uint64 {
	m := map[string]uint64{}
	s := st.Stats()
	m["FramesIn"], m["FramesOut"], m["FramesDropped"] = s.FramesIn, s.FramesOut, s.FramesDropped
	m["FramesCRCDropped"], m["GuestFramesCRCDropped"] = s.FramesCRCDropped, s.GuestFramesCRCDropped
	m["TCPDenied"], m["TCPRefused"], m["TCPActive"] = s.TCPDenied, s.TCPRefused, uint64(s.TCPActive) //nolint:gosec // G115: TCPActive is a connection count, never negative
	g := st.GVisorStats()
	walkStats("", reflect.ValueOf(&g).Elem(), m)
	return m
}

var statCounterType = reflect.TypeOf((*tcpip.StatCounter)(nil))

// walkStats collects every *tcpip.StatCounter below v, by field path.
func walkStats(prefix string, v reflect.Value, m map[string]uint64) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		if v.Type() == statCounterType {
			m[prefix] = v.Interface().(*tcpip.StatCounter).Value()
			return
		}
		walkStats(prefix, v.Elem(), m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			if prefix != "" {
				name = prefix + "." + name
			}
			walkStats(name, v.Field(i), m)
		}
	}
}
