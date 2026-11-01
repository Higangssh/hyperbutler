package notelemetry

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Finding is one use of a network API.
type Finding struct {
	File string // slash-separated, relative to the scan root
	Line int
	// Rule names the API: an import path and identifier ("net/http.Client"), or a
	// kind and value ("exec:curl", "dll:winhttp.dll", "url:example.com", "sh:ntpd").
	Rule string
}

func (f Finding) String() string { return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Rule) }

// all marks every exported identifier of a package as a network API.
const all = "*"

// networkAPIs are the identifiers ScanGo reports, by import path.
var networkAPIs = map[string][]string{
	"net": {"Dial", "DialTimeout", "DialTCP", "DialUDP", "DialIP", "Dialer", "Resolver", "DefaultResolver",
		"LookupAddr", "LookupCNAME", "LookupHost", "LookupIP", "LookupMX", "LookupNS", "LookupPort",
		"LookupSRV", "LookupTXT", "LookupNetIP", "ListenMulticastUDP"},
	"net/http": {"Client", "Transport", "DefaultClient", "DefaultTransport", "Get", "Head", "Post",
		"PostForm", "ProxyFromEnvironment", "ProxyURL"},
	"net/http/httputil":          {"ReverseProxy", "NewSingleHostReverseProxy"},
	"crypto/tls":                 {"Dial", "DialWithDialer", "Dialer"},
	"net/smtp":                   {"Dial", "SendMail", "NewClient"},
	"net/rpc":                    {"Dial", "DialHTTP", "DialHTTPPath"},
	"net/rpc/jsonrpc":            {"Dial"},
	"github.com/miekg/dns":       {"Exchange", "ExchangeContext", "ExchangeConn", "Client", "Dial", "DialTimeout", "DialWithTLS", "DialTimeoutWithTLS"},
	"golang.org/x/net/proxy":     {all},
	"golang.org/x/net/websocket": {all},
	"golang.org/x/sys/unix":      {"Connect", "Socket", "Sendto", "Sendmsg", "SendmsgN"},
	"syscall":                    {"Connect", "Socket", "Sendto", "Sendmsg", "SendmsgN", "WSASendto", "WSAConnect"},
	"golang.org/x/sys/windows":   {"Connect", "Socket", "WSASocket", "Sendto", "WSASendto", "WSAConnect"},
}

// networkTools are programs that reach the network; exec'ing one, or running one from
// a guest init script, is reported.
var networkTools = map[string]bool{
	"curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true, "telnet": true, "ssh": true,
	"scp": true, "ftp": true, "ftpget": true, "ftpput": true, "tftp": true, "ping": true, "ping6": true,
	"nslookup": true, "dig": true, "traceroute": true, "whois": true, "rdate": true,
	"ntpd": true, "ntpdate": true, "chronyd": true, "sntp": true, "udhcpc": true, "dhcpcd": true,
	"dhclient": true, "bitsadmin": true, "certutil": true, "invoke-webrequest": true, "invoke-restmethod": true,
}

// networkDLLs are Windows client libraries for HTTP and URL downloads.
var networkDLLs = []string{"winhttp", "wininet", "urlmon"}

// localHosts are the URL hosts that do not leave the machine: the Docker API over the
// guest's hvsock and the debug endpoint over a pipe both use these as dummy hosts.
var localHosts = map[string]bool{"docker": true, "diag": true, "localhost": true}

// ScanGo reports every network API that the non-test Go files under root/dirs use.
// Directories named testdata or vendor, and hidden ones, are skipped.
func ScanGo(root string, dirs []string) ([]Finding, error) {
	var out []Finding
	fset := token.NewFileSet()
	for _, d := range dirs {
		base := filepath.Join(root, filepath.FromSlash(d))
		err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				n := e.Name()
				if p != base && (n == "testdata" || n == "vendor" || strings.HasPrefix(n, ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			out = append(out, scanFile(fset, filepath.ToSlash(rel), f)...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

func scanFile(fset *token.FileSet, rel string, f *ast.File) []Finding {
	// Local name -> import path, for the packages that have network APIs.
	imports := map[string]string{}
	execName := ""
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		name := path.Base(p)
		if im.Name != nil {
			name = im.Name.Name
		}
		if _, ok := networkAPIs[p]; ok {
			imports[name] = p
		}
		if p == "os/exec" {
			execName = name
		}
	}
	var out []Finding
	add := func(pos token.Pos, rule string) {
		out = append(out, Finding{File: rel, Line: fset.Position(pos).Line, Rule: rule})
	}
	pkgOf := func(e ast.Expr) (pkg, sel string, ok bool) {
		s, ok := e.(*ast.SelectorExpr)
		if !ok {
			return "", "", false
		}
		id, ok := s.X.(*ast.Ident)
		if !ok {
			return "", "", false
		}
		if p, ok := imports[id.Name]; ok {
			return p, s.Sel.Name, true
		}
		if id.Name == execName && execName != "" {
			return "os/exec", s.Sel.Name, true
		}
		return "", "", false
	}
	exempt := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			pkg, sel, ok := pkgOf(n.Fun)
			if !ok {
				return true
			}
			if pkg == "os/exec" && (sel == "Command" || sel == "CommandContext") {
				arg := 0
				if sel == "CommandContext" {
					arg = 1
				}
				if len(n.Args) > arg {
					if s, ok := stringLit(n.Args[arg]); ok {
						tool := strings.ToLower(strings.TrimSuffix(path.Base(strings.ReplaceAll(s, `\`, "/")), ".exe"))
						if networkTools[tool] {
							add(n.Pos(), "exec:"+tool)
						}
					}
				}
				return true
			}
			if localCall(pkg, sel, n.Args, imports) {
				exempt[n.Fun] = true
			}
		case *ast.SelectorExpr:
			if exempt[n] {
				return true
			}
			pkg, sel, ok := pkgOf(n)
			if !ok || pkg == "os/exec" || !ast.IsExported(sel) {
				return true
			}
			for _, name := range networkAPIs[pkg] {
				if name == sel || name == all {
					add(n.Pos(), pkg+"."+sel)
					break
				}
			}
		case *ast.BasicLit:
			s, ok := stringLit(n)
			if !ok {
				return true
			}
			low := strings.ToLower(s)
			for _, d := range networkDLLs {
				if strings.Contains(low, d+".dll") || low == d {
					add(n.Pos(), "dll:"+d+".dll")
				}
			}
			if h, ok := urlHost(s); ok && !localHost(h) {
				add(n.Pos(), "url:"+h)
			}
		}
		return true
	})
	return out
}

// localCall reports whether a call to a network API is local by its literal arguments:
// net.Dial or net.DialTimeout on a Unix socket or a loopback address, a raw socket of
// family AF_VSOCK, AF_UNIX or AF_NETLINK, or a connect to a SockaddrVM or SockaddrUnix.
func localCall(pkg, sel string, args []ast.Expr, imports map[string]string) bool {
	switch {
	case pkg == "net" && (sel == "Dial" || sel == "DialTimeout"):
		if len(args) < 2 {
			return false
		}
		network, ok := stringLit(args[0])
		if !ok {
			return false
		}
		if strings.HasPrefix(network, "unix") {
			return true
		}
		addr, ok := stringLit(args[1])
		if !ok {
			return false
		}
		host, _, err := splitHostPort(addr)
		return err == nil && localHost(host)
	case sel == "Socket" && (pkg == "golang.org/x/sys/unix" || pkg == "syscall" || pkg == "golang.org/x/sys/windows"):
		if len(args) < 1 {
			return false
		}
		p, name, ok := selectorIn(args[0], imports)
		return ok && p == pkg && (name == "AF_VSOCK" || name == "AF_UNIX" || name == "AF_NETLINK")
	case sel == "Connect" && (pkg == "golang.org/x/sys/unix" || pkg == "syscall" || pkg == "golang.org/x/sys/windows"):
		if len(args) < 2 {
			return false
		}
		u, ok := args[1].(*ast.UnaryExpr)
		if !ok || u.Op != token.AND {
			return false
		}
		cl, ok := u.X.(*ast.CompositeLit)
		if !ok {
			return false
		}
		p, name, ok := selectorIn(cl.Type, imports)
		return ok && p == pkg && (name == "SockaddrVM" || name == "SockaddrUnix")
	}
	return false
}

func selectorIn(e ast.Expr, imports map[string]string) (pkg, name string, ok bool) {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	id, ok := s.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	p, ok := imports[id.Name]
	return p, s.Sel.Name, ok
}

func stringLit(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	return s, err == nil
}

func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return "", "", fmt.Errorf("no port in %q", addr)
	}
	host, port = addr[:i], addr[i+1:]
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return host, port, nil
}

// urlHost returns the host of a string that starts with a URL scheme that reaches a
// server (http, https, ws, wss, ftp).
func urlHost(s string) (string, bool) {
	for _, scheme := range []string{"http://", "https://", "ws://", "wss://", "ftp://"} {
		if len(s) > len(scheme) && strings.EqualFold(s[:len(scheme)], scheme) {
			u, err := url.Parse(s)
			if err != nil || u.Host == "" {
				// Not parseable: report what follows the scheme up to the path.
				h, _, _ := strings.Cut(s[len(scheme):], "/")
				return strings.ToLower(h), true
			}
			return strings.ToLower(u.Hostname()), true
		}
	}
	return "", false
}

func localHost(h string) bool {
	if localHosts[strings.ToLower(h)] {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Unmap().IsLoopback()
}

// shellRules are the words a guest init script may not contain (outside comments): the
// network tools, and what turns on dockerd's metrics endpoint or OpenTelemetry export.
var shellRules = regexp.MustCompile(`(?i)metrics-addr|OTEL_[A-Z_]*`)

// shellWord splits a shell line into words for the network tool check.
var shellWord = regexp.MustCompile(`[A-Za-z0-9_.-]+`)

// ScanShell reports network tools and telemetry switches in shell scripts (paths
// relative to root). Comments, from a # at the start of a word to the end of the
// line, are ignored.
func ScanShell(root string, files []string) ([]Finding, error) {
	var out []Finding
	for _, rel := range files {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // G304: the caller names the repository files to scan
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		line := 0
		for sc.Scan() {
			line++
			text := stripComment(sc.Text())
			for _, w := range shellWord.FindAllString(text, -1) {
				if networkTools[strings.ToLower(w)] {
					out = append(out, Finding{File: rel, Line: line, Rule: "sh:" + strings.ToLower(w)})
				}
			}
			for _, m := range shellRules.FindAllString(text, -1) {
				out = append(out, Finding{File: rel, Line: line, Rule: "sh:" + m})
			}
		}
		err = sc.Err()
		_ = f.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// stripComment drops a shell comment: a # that starts a word, outside quotes. "$#"
// and "${#x}" are not comments.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				i++
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '\\':
			i++
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == ';'):
			return s[:i]
		}
	}
	return s
}
