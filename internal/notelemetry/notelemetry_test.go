package notelemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// productionDirs are scanned by TestNoUnreviewedNetworkUse (see the package doc).
var productionDirs = []string{"cmd/hyperbutler", "cmd/hyperbutler-broker", "internal", "guest/cmd", "guest/internal"}

// guestScripts are the guest's init scripts (ADR 0005), run as PID 1 in the VM.
var guestScripts = []string{"guest/prototype/init", "guest/prototype/initramfs-init"}

// shippedBinaries are the binaries in the release zip and the guest image, by GOOS.
var shippedBinaries = map[string][]string{
	"windows": {"./cmd/hyperbutler", "./cmd/hyperbutler-broker"},
	"linux":   {"./guest/cmd/agent", "./guest/cmd/sockfwd", "./guest/cmd/netfwd", "./guest/cmd/hyperbutlerd"},
}

// reviewedModules are the modules allowed in shippedBinaries, with why none of them
// makes a network request on its own. A new module needs a line here (and a look at
// whether it starts anything at init, dials, or reports anywhere).
var reviewedModules = map[string]string{
	"github.com/Higangssh/hyperbutler":       "this repository; its own code is scanned by ScanGo",
	"github.com/containers/gvisor-tap-vsock": "only its tap.LinkEndpoint (the gateway NIC) is used; it dials nothing",
	"github.com/miekg/dns":                   "DNS message packing for the guest's DNS forwarder; its client (Exchange, Client) is a reported API",
	"golang.org/x/sys":                       "system call wrappers; connect and socket calls are reported by ScanGo",
	"gvisor.dev/gvisor":                      "the user-mode TCP/IP stack, which has only the guest NIC: it can reach the guest, nothing else",
	"github.com/google/btree":                "data structure (gvisor)",
	"github.com/google/gopacket":             "packet parsing (gvisor-tap-vsock)",
	"github.com/sirupsen/logrus":             "logging to a local writer (gvisor-tap-vsock)",
	"golang.org/x/exp":                       "generic helpers",
	"golang.org/x/time":                      "rate limiting (gvisor)",
	// hyperbutler mcp (REQ-UX-012). internal/mcpserver builds only an mcp.IOTransport on
	// stdin and stdout; the SDK's HTTP transports and OAuth client (with x/oauth2) are
	// linked but run only when constructed, and none of these packages dials in init.
	"github.com/modelcontextprotocol/go-sdk": "the MCP SDK: JSON-RPC over the stdio transport hyperbutler constructs; its HTTP client and server transports are never constructed",
	"golang.org/x/oauth2":                    "token types for the MCP SDK's auth package; hyperbutler runs no OAuth flow",
	"github.com/google/jsonschema-go":        "JSON Schema inference and validation of tool arguments (MCP SDK)",
	"github.com/segmentio/encoding":          "JSON decoding (MCP SDK)",
	"github.com/segmentio/asm":               "SIMD helpers for segmentio/encoding",
	"github.com/yosida95/uritemplate/v3":     "URI templates for MCP resources (MCP SDK); hyperbutler serves none",
	"golang.org/x/sync":                      "errgroup (MCP SDK)",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found at %s: %v", root, err)
	}
	return root
}

// allowEntry is one line of allowlist.txt: a file, a rule, how many uses of it the
// file has, and the reason.
type allowEntry struct {
	file, rule, why string
	count, line     int
}

func readAllowlist(t *testing.T) []allowEntry {
	t.Helper()
	f, err := os.Open("allowlist.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []allowEntry
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		fields := strings.Fields(s)
		count := 0
		if len(fields) >= 4 {
			count, _ = strconv.Atoi(fields[2])
		}
		if count < 1 {
			t.Fatalf("allowlist.txt:%d: want FILE RULE COUNT REASON, got %q", n, s)
		}
		out = append(out, allowEntry{file: fields[0], rule: fields[1], count: count, why: strings.Join(fields[3:], " "), line: n})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNoUnreviewedNetworkUse fails on a network API in production code that
// allowlist.txt does not name, on more (or fewer) uses of it in a file than the entry
// counts, and on an allowlist entry that matches nothing. The count keeps one reviewed
// net.Dialer from covering the next one added to the same file.
func TestNoUnreviewedNetworkUse(t *testing.T) {
	root := repoRoot(t)
	fs, err := ScanGo(root, productionDirs)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range checkAllowlist(fs, readAllowlist(t)) {
		t.Error(p)
	}
}

// checkAllowlist returns what is wrong with fs against allow.
func checkAllowlist(fs []Finding, allow []allowEntry) (problems []string) {
	errorf := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	type key struct{ file, rule string }
	found := map[key][]Finding{}
	var order []key
	for _, f := range fs {
		if strings.HasPrefix(f.File, "internal/notelemetry/") {
			continue // the checker's own tables name the APIs it looks for
		}
		k := key{f.File, f.Rule}
		if found[k] == nil {
			order = append(order, k)
		}
		found[k] = append(found[k], f)
	}
	listed := map[key]allowEntry{}
	for _, a := range allow {
		k := key{a.file, a.rule}
		if _, dup := listed[k]; dup {
			errorf("allowlist.txt:%d: %s %s is listed twice", a.line, a.file, a.rule)
		}
		listed[k] = a
		if len(found[k]) == 0 {
			errorf("allowlist.txt:%d: %s %s matches nothing; remove it", a.line, a.file, a.rule)
		}
	}
	for _, k := range order {
		a, ok := listed[k]
		uses := found[k]
		switch {
		case !ok:
			for _, f := range uses {
				errorf("%s: a network API in production code (REQ-UX-005). If it cannot reach the network on its own, add \"%s %s %d <why>\" to internal/notelemetry/allowlist.txt", f, f.File, f.Rule, len(uses))
			}
		case len(uses) != a.count:
			var lines []string
			for _, f := range uses {
				lines = append(lines, strconv.Itoa(f.Line))
			}
			errorf("allowlist.txt:%d: %s %s allows %d uses, the file has %d (lines %s): review the new one, then update the count",
				a.line, a.file, a.rule, a.count, len(uses), strings.Join(lines, ", "))
		}
	}
	return problems
}

// TestGuestScriptsStayOffline fails if the guest's init runs a network tool or turns on
// dockerd's metrics or OpenTelemetry export.
func TestGuestScriptsStayOffline(t *testing.T) {
	fs, err := ScanShell(repoRoot(t), guestScripts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		t.Errorf("%s: the guest init must not reach the network or export telemetry by itself (REQ-UX-005)", f)
	}
}

// TestLinkedModulesReviewed fails when a shipped binary links a module that is not in
// reviewedModules, or when an entry there is no longer linked.
func TestLinkedModulesReviewed(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command in PATH")
	}
	root := repoRoot(t)
	linked := map[string][]string{} // module -> GOOS of the binaries that link it
	for _, goos := range []string{"windows", "linux"} {
		args := append([]string{"list", "-deps", "-json=ImportPath,Standard,Module"}, shippedBinaries[goos]...)
		cmd := exec.Command(gobin, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list (%s): %v: %s", goos, err, stderr.String())
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var p struct {
				ImportPath string
				Standard   bool
				Module     *struct{ Path string }
			}
			if err := dec.Decode(&p); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if p.Standard || p.Module == nil {
				continue
			}
			m := p.Module.Path
			if l := linked[m]; len(l) == 0 || l[len(l)-1] != goos {
				linked[m] = append(linked[m], goos)
			}
		}
	}
	var mods []string
	for m := range linked {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	for _, m := range mods {
		if _, ok := reviewedModules[m]; !ok {
			t.Errorf("module %s is linked into the shipped %s binaries but not reviewed: check that it makes no network request of its own (REQ-UX-005), then add it to reviewedModules", m, strings.Join(linked[m], ", "))
		}
	}
	for m := range reviewedModules {
		if _, ok := linked[m]; !ok {
			t.Errorf("reviewedModules lists %s, which no shipped binary links; remove it", m)
		}
	}
}
