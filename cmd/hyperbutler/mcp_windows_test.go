//go:build windows

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpSession runs `hyperbutler mcp args...` on pipes and connects a client to it.
func mcpSession(t *testing.T, args ...string) *mcp.ClientSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		var errb strings.Builder
		err := cmdMCP(args, inR, outW, &errb)
		_ = outW.Close()
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = inW.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("mcp: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("mcp did not exit after stdin closed")
		}
		cancel()
	})
	return cs
}

func mcpCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), r.IsError
}

func TestMCPReadOnlyAgainstStoppedEngine(t *testing.T) {
	home, _ := testHome(t)
	t.Setenv("TMP", t.TempDir())
	cs := mcpSession(t)
	s, isErr := mcpCall(t, cs, "status", nil)
	var st StatusJSON
	if isErr || json.Unmarshal([]byte(s), &st) != nil || st.Schema != SchemaVersion || st.Running || !strings.EqualFold(st.Home, home) {
		t.Errorf("status: %v %s", isErr, s)
	}
	s, isErr = mcpCall(t, cs, "settings", nil)
	var cl ConfigListJSON
	if isErr || json.Unmarshal([]byte(s), &cl) != nil || len(cl.Keys) == 0 {
		t.Errorf("settings: %v %s", isErr, s)
	}
	if s, isErr := mcpCall(t, cs, "containers", nil); !isErr || !strings.Contains(s, "not running") || !strings.Contains(s, "--allow-write") {
		t.Errorf("containers: %v %s", isErr, s)
	}
	if s, isErr := mcpCall(t, cs, "engine_logs", nil); !isErr || !strings.Contains(s, "no log yet") {
		t.Errorf("engine_logs: %v %s", isErr, s)
	}
	s, isErr = mcpCall(t, cs, "diagnose", nil)
	var d DiagnoseJSON
	if isErr || json.Unmarshal([]byte(s), &d) != nil || !strings.EqualFold(filepath.Dir(d.File), filepath.Clean(os.Getenv("TMP"))) || len(d.Files) == 0 {
		t.Errorf("diagnose: %v %s", isErr, s)
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "config_set", Arguments: map[string]any{"key": "cpus", "value": "2"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("config_set without --allow-write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "settings.json")); err == nil {
		t.Error("a read-only server wrote settings.json")
	}
}

func TestMCPConfigSet(t *testing.T) {
	home, _ := testHome(t)
	cs := mcpSession(t, "--allow-write")
	if s, isErr := mcpCall(t, cs, "config_set", map[string]any{"key": "cpus", "value": "2"}); isErr || !strings.Contains(s, "cpus") {
		t.Errorf("config_set cpus: %v %s", isErr, s)
	}
	if s, isErr := mcpCall(t, cs, "config_set", map[string]any{"key": "cpus", "value": "--apply"}); !isErr || !strings.Contains(s, "cpus") {
		t.Errorf("config_set cpus --apply: %v %s", isErr, s)
	}
	if s, isErr := mcpCall(t, cs, "config_set", map[string]any{"key": "proxy.http", "value": "http://u:p@proxy:3128"}); !isErr || strings.Contains(s, "u:p@") {
		t.Errorf("config_set proxy.http: %v %s", isErr, s)
	}
	b, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil || !strings.Contains(string(b), `"cpus": 2`) || strings.Contains(string(b), "proxy") {
		t.Errorf("settings.json: %v %s", err, b)
	}
}

func TestMCPFlags(t *testing.T) {
	testHome(t)
	for _, args := range [][]string{
		{"mcp", "--allow-delete"},
		{"mcp", "--allow-persistent"},
		{"mcp", "--allow-registry=ghcr.io"},
	} {
		if code, _, errs := runCLI(t, args...); code != 2 || !strings.Contains(errs, "--allow-write") {
			t.Errorf("%v: %d %q", args, code, errs)
		}
	}
	for _, bad := range []string{"192.168.0.4:5000", "host.docker.internal", "localhost:5000"} {
		if code, _, errs := runCLI(t, "mcp", "--allow-write", "--allow-registry="+bad); code != 2 || !strings.Contains(errs, "refused") {
			t.Errorf("--allow-registry=%s: %d %q", bad, code, errs)
		}
	}
}

// The MCP status tool, and diagnose, make no update check (REQ-UX-022): a read-only
// tool makes no network request. The CLI's status still does.
func TestMCPStatusMakesNoUpdateCheck(t *testing.T) {
	testHome(t)
	t.Setenv("TMP", t.TempDir())
	api := newReleaseAPI(t, http.StatusOK, `{"tag_name":"v9.0.0","html_url":"https://github.com/Higangssh/hyperbutler/releases/tag/v9.0.0"}`)
	if code, _, _ := runCLI(t, "config", "set", "memory", "4GiB"); code != 0 {
		t.Fatal("config set")
	}
	cs := mcpSession(t)
	for _, tool := range []string{"status", "diagnose", "disk_usage", "memory"} {
		if s, isErr := mcpCall(t, cs, tool, nil); isErr {
			t.Errorf("%s: %s", tool, s)
		}
	}
	if n := api.count(); n != 0 {
		t.Fatalf("the MCP read tools made %d update checks", n)
	}
	// The record of the CLI's last check is shown, without a new one.
	runCLI(t, "status")
	if api.count() != 1 {
		t.Fatalf("the CLI's status made %d checks", api.count())
	}
	s, _ := mcpCall(t, cs, "status", nil)
	var st StatusJSON
	if err := json.Unmarshal([]byte(s), &st); err != nil || st.Update == nil || st.Update.Latest != "v9.0.0" || api.count() != 1 {
		t.Errorf("status after a CLI check: %v %s (%d checks)", err, s, api.count())
	}
}
