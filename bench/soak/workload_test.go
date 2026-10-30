package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchUntil(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			http.NotFound(w, r) // the listener is up before the page is right
			return
		}
		fmt.Fprint(w, "soak-1 page\n")
	}))
	defer srv.Close()
	if err := fetchUntil(context.Background(), srv.URL, "soak-1 page\n", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// A wrong page fails at once; nothing listening fails after the wait.
	if err := fetchUntil(context.Background(), srv.URL, "other\n", 5*time.Second); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("wrong page: %v", err)
	}
	srv.Close()
	t0 := time.Now()
	if err := fetchUntil(context.Background(), srv.URL, "x", 500*time.Millisecond); err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("closed server: %v after %s", err, time.Since(t0))
	}
}

func TestTotals(t *testing.T) {
	var w Workload
	w.record("version", nil, "")
	w.record("rm", errors.New("boom"), "detail")
	ops, errs, byKind, failures := w.Totals()
	if ops != 2 || errs != 1 || strings.Contains(byKind, "bind") || !strings.Contains(byKind, "rm=0/1") || len(failures) != 1 {
		t.Fatalf("%d %d %q %v", ops, errs, byKind, failures)
	}
	w.record("bind", nil, "")
	w.record("http", errors.New("timeout"), "")
	if _, errs, byKind, _ := w.Totals(); errs != 2 || !strings.Contains(byKind, "bind=1/1") || !strings.Contains(byKind, "http=0/1") {
		t.Fatalf("%q", byKind)
	}
}
