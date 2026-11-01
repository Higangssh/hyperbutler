//go:build windows

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var failed int

func check(name string, ok bool, detail string) {
	if ok {
		fmt.Println("PASS", name)
		return
	}
	failed++
	fmt.Println("FAIL", name, strings.TrimSpace(detail))
}

type result struct {
	code int
	out  string
}

func run(exe string, args ...string) result {
	cmd := exec.Command(exe, args...) //nolint:gosec // G204: the bench runs the programs it was told to
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	return result{code, b.String()}
}

func main() {
	hbExe := flag.String("hyperbutler", "hyperbutler.exe", "the hyperbutler CLI")
	dockerExe := flag.String("docker", "docker.exe", "the docker CLI")
	image := flag.String("image", "busybox:1.37", "an image on Docker Hub to pull through the proxy")
	flag.Parse()

	var transcript strings.Builder // every hyperbutler and docker output, for the leak check
	hb := func(args ...string) result {
		r := run(*hbExe, args...)
		transcript.WriteString(r.out)
		return r
	}
	dk := func(args ...string) result {
		r := run(*dockerExe, args...)
		transcript.WriteString(r.out)
		return r
	}

	if r := hb("status"); r.code != 0 {
		fmt.Fprintln(os.Stderr, "hyperbutler is not running:", r.out)
		os.Exit(2)
	}
	ca, err := newTestCA()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	dir, err := os.MkdirTemp("", "hb-corp-proxy-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	caFile := filepath.Join(dir, "corp-ca.pem")
	if err := os.WriteFile(caFile, ca.pem, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var pwb [12]byte
	_, _ = rand.Read(pwb[:])
	pw := "pw" + hex.EncodeToString(pwb[:])
	// The proxy reaches Docker Hub directly, verifying it with the Windows roots.
	upstream := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: false,
		ResponseHeaderTimeout: 60 * time.Second, MaxIdleConnsPerHost: 4}
	p := &mitmProxy{ca: ca, user: "corpuser", pw: pw, upstream: upstream}
	if err := p.start("127.0.0.1:0"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer p.close()
	proxyURL := "http://corpuser:" + pw + "@" + p.addr()
	fmt.Println("proxy on", p.addr(), "(credentials required); CA in", caFile)

	cleanup := func() {
		hb("config", "unset", "proxy.http")
		hb("config", "unset", "proxy.https")
		hb("config", "unset", "ca.extraFile")
	}
	defer cleanup()

	// 1. The proxy, no CA.
	hb("config", "unset", "ca.extraFile")
	r1 := hb("config", "set", "proxy.http", proxyURL)
	r2 := hb("config", "set", "proxy.https", proxyURL)
	check("set proxy.http and proxy.https", r1.code == 0 && r2.code == 0, r1.out+r2.out)
	r := hb("config", "daemon", "apply")
	check("apply with the proxy", r.code == 0, r.out)
	r = hb("config", "set", "hostGateway.enabled", "false")
	check("hostGateway.enabled false refused under a loopback proxy", r.code != 0 && strings.Contains(r.out, "loopback"), r.out)
	dk("rmi", "-f", *image)
	before := p.stats()
	r = dk("pull", *image)
	after := p.stats()
	check("pull without the CA fails with an x509 error", r.code != 0 && (strings.Contains(r.out, "x509") || strings.Contains(r.out, "certificate")), r.out)
	check("the proxy saw CONNECT registry-1.docker.io:443", containsFrom(after.Connects, len(before.Connects), "registry-1.docker.io:443"), fmt.Sprint(after.Connects))
	check("dockerd refused the proxy's certificate", after.TLSFailed > before.TLSFailed && after.TLSOK == before.TLSOK, fmt.Sprintf("%+v", after))

	// 2. With the proxy's CA.
	r = hb("config", "set", "ca.extraFile", caFile, "--apply")
	check("set ca.extraFile --apply", r.code == 0, r.out)
	before = p.stats()
	t0 := time.Now()
	r = dk("pull", *image)
	after = p.stats()
	check("pull with the CA succeeds", r.code == 0, r.out)
	fmt.Printf("     pulled in %.1f s; proxy: %d CONNECTs, %d TLS OK, %d requests\n", time.Since(t0).Seconds(),
		len(after.Connects)-len(before.Connects), after.TLSOK-before.TLSOK, after.Req-before.Req)
	check("the pull went through the proxy, inspected", after.TLSOK > before.TLSOK && after.Req > before.Req, fmt.Sprintf("%+v", after))
	check("the proxy's credentials were accepted", after.AuthFailed == before.AuthFailed, fmt.Sprintf("%d failures", after.AuthFailed))

	// 3. In the guest and in containers.
	gw := dk("run", "--rm", "--privileged", "--pid=host", *image, "nsenter", "-t", "1", "-m", "--", "sh", "-c",
		"stat -c '%a %n' /run/hyperbutler /run/hyperbutler/dockerd.env; cut -d= -f1 /run/hyperbutler/dockerd.env; grep -c '@192.168.127.254:' /run/hyperbutler/dockerd.env; grep -c 'hyperbutler bench' /run/hyperbutler/ca-certificates.crt || true")
	check("guest: dockerd.env 0600 in a 0700 directory", strings.Contains(gw.out, "700 /run/hyperbutler\n") && strings.Contains(gw.out, "600 /run/hyperbutler/dockerd.env"), gw.out)
	check("guest: HTTP_PROXY, HTTPS_PROXY, NO_PROXY, SSL_CERT_FILE set; the proxy via the host gateway", strings.Contains(gw.out, "HTTPS_PROXY\nNO_PROXY\nSSL_CERT_FILE\n") && strings.Contains(gw.out, "\n2\n"), gw.out)
	env := dk("run", "--rm", *image, "env")
	check("containers get no proxy variables", env.code == 0 && !strings.Contains(strings.ToUpper(env.out), "PROXY="), env.out)
	cmdline := dk("run", "--rm", *image, "cat", "/proc/cmdline")
	check("/proc/cmdline has no proxy", cmdline.code == 0 && !strings.Contains(cmdline.out, "corpuser"), cmdline.out)
	info := dk("info", "--format", "{{.HTTPSProxy}}")
	check("docker info shows the proxy masked", info.code == 0 && strings.Contains(info.out, "192.168.127.254") && !strings.Contains(info.out, pw), info.out)

	// 4. Nothing printed or stored shows the password.
	hb("config", "list")
	hb("config", "list", "--json")
	hb("config", "get", "proxy.https")
	hb("status")
	hb("status", "--json")
	hb("logs", "-n", "0")
	hb("logs", "--console", "-n", "0")
	var paths struct {
		Home string `json:"home"`
	}
	if r := run(*hbExe, "config", "path", "--json"); json.Unmarshal([]byte(r.out), &paths) == nil {
		if b, err := os.ReadFile(filepath.Join(paths.Home, "state.json")); err == nil {
			transcript.Write(b)
		}
	}
	check("the password is in no output, log or state file", !strings.Contains(transcript.String(), pw), "")
	check("the redacted form is shown", strings.Contains(transcript.String(), "corpuser:***@"), "")
	shown := run(*hbExe, "config", "get", "--show-secrets", "proxy.https")
	check("config get --show-secrets shows it", strings.TrimSpace(shown.out) == proxyURL, "")

	// 5. Back to no proxy.
	cleanup()
	r = hb("config", "daemon", "apply")
	check("apply without the proxy", r.code == 0, r.out)
	dk("rmi", "-f", *image)
	before = p.stats()
	r = dk("pull", *image)
	check("pull without the proxy works directly", r.code == 0 && len(p.stats().Connects) == len(before.Connects), r.out)

	if failed > 0 {
		fmt.Printf("%d check(s) failed\n", failed)
		p.close()
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	fmt.Println("all checks passed")
}

func containsFrom(l []string, from int, s string) bool {
	for _, v := range l[from:] {
		if v == s {
			return true
		}
	}
	return false
}
