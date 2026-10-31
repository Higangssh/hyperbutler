//go:build windows

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type bench struct {
	hb, docker, image string
	wildcard          bool
	failed            int
	token             string
}

func main() {
	var b bench
	fs := flag.NewFlagSet("host-gateway", flag.ExitOnError)
	fs.StringVar(&b.hb, "hyperbutler", "hyperbutler", "hyperbutler CLI")
	fs.StringVar(&b.docker, "docker", "docker", "docker CLI (uses DOCKER_HOST)")
	fs.StringVar(&b.image, "image", "busybox:1.37", "container image with wget, nc and nslookup")
	fs.BoolVar(&b.wildcard, "wildcard", false, "also run a server on 0.0.0.0 to check that the host's LAN address stays refused (Windows Firewall may ask)")
	_ = fs.Parse(os.Args[1:])
	tok := make([]byte, 6)
	_, _ = rand.Read(tok)
	b.token = hex.EncodeToString(tok)
	if err := b.run(); err != nil {
		fmt.Fprintln(os.Stderr, "host-gateway:", err)
		os.Exit(1)
	}
	if b.failed > 0 {
		fmt.Printf("%d check(s) failed\n", b.failed)
		os.Exit(1)
	}
	fmt.Println("all checks passed")
}

func (b *bench) check(name string, ok bool, detail string) {
	if ok {
		fmt.Println("PASS", name)
		return
	}
	b.failed++
	fmt.Println("FAIL", name, strings.TrimSpace(detail))
}

type result struct {
	out  string
	err  error
	took time.Duration
}

func (r result) String() string { return fmt.Sprintf("(%v) %s", r.err, strings.TrimSpace(r.out)) }

func run(exe string, args ...string) result {
	t := time.Now()
	cmd := exec.Command(exe, args...) //nolint:gosec // G204: command and arguments come from the operator's own flags or this tool's work directory; exec passes argv, no shell
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return result{out: out.String(), err: err, took: time.Since(t)}
}

func (b *bench) dk(args ...string) result  { return run(b.docker, args...) }
func (b *bench) hbc(args ...string) result { return run(b.hb, args...) }

// sh runs a shell command in a fresh container.
func (b *bench) sh(extra []string, script string) result {
	args := append([]string{"run", "--rm"}, extra...)
	return b.dk(append(args, b.image, "sh", "-c", script)...)
}

// wget fetches url from a container; it reports the body.
func (b *bench) wget(extra []string, url string) result {
	return b.sh(extra, "wget -q -T 5 -O - "+url)
}

type status struct {
	Running bool `json:"running"`
	State   *struct {
		PID         int    `json:"pid"`
		VMID        string `json:"vmId"`
		HostGateway *struct {
			Address string `json:"address"`
			Enabled bool   `json:"enabled"`
			Ports   string `json:"ports"`
		} `json:"hostGateway"`
	} `json:"state"`
}

func (b *bench) status() (*status, error) {
	r := b.hbc("status", "--json")
	var s status
	if err := json.Unmarshal([]byte(r.out), &s); err != nil {
		return nil, fmt.Errorf("status --json: %w: %s", err, r.out)
	}
	return &s, nil
}

func (b *bench) run() error {
	st, err := b.status()
	if err != nil {
		return err
	}
	if !st.Running || st.State == nil || st.State.HostGateway == nil {
		return fmt.Errorf("hyperbutler is not running with a host gateway (status: %+v)", st)
	}
	hgw := st.State.HostGateway.Address
	vm0 := st.State.VMID
	fmt.Printf("hyperbutler: engine pid %d, VM %s, host gateway %s (%s)\n", st.State.PID, vm0, hgw, st.State.HostGateway.Ports)
	if r := b.dk("pull", "-q", b.image); r.err != nil {
		return fmt.Errorf("pull %s: %s", b.image, r)
	}
	defer func() {
		// Leave the settings as the bench found them (the defaults).
		b.hbc("config", "unset", "hostGateway.enabled")
		b.hbc("config", "unset", "hostGateway.ports")
		b.dk("network", "rm", "hb-hostgw-net")
	}()

	v4, err := startHTTP("tcp4", "127.0.0.1:0", "hb-hostgw-v4-"+b.token)
	if err != nil {
		return err
	}
	defer v4.close()
	other, err := startHTTP("tcp4", "127.0.0.1:0", "hb-hostgw-other-"+b.token)
	if err != nil {
		return err
	}
	defer other.close()
	p4, po := strconv.Itoa(v4.port()), strconv.Itoa(other.port())
	want4 := "hb-hostgw-v4-" + b.token

	// 1. The name, the alias and --add-host, on the default bridge.
	r := b.wget(nil, "http://host.docker.internal:"+p4+"/")
	b.check("host.docker.internal:"+p4+" reaches 127.0.0.1:"+p4, r.err == nil && strings.Contains(r.out, want4), r.String())
	base := r.took
	fmt.Printf("     (docker run + wget took %v)\n", base.Round(time.Millisecond))
	r = b.wget(nil, "http://gateway.docker.internal:"+p4+"/")
	b.check("gateway.docker.internal reaches it too", r.err == nil && strings.Contains(r.out, want4), r.String())
	r = b.sh(nil, "nslookup host.docker.internal")
	b.check("nslookup host.docker.internal gives "+hgw, r.err == nil && strings.Contains(r.out, hgw), r.String())
	r = b.sh([]string{"--add-host", "foo:host-gateway"}, "grep -w foo /etc/hosts; wget -q -T 5 -O - http://foo:"+p4+"/")
	b.check("--add-host foo:host-gateway maps foo to "+hgw+" and reaches the host", r.err == nil && strings.Contains(r.out, hgw) && strings.Contains(r.out, want4), r.String())

	// 2. A user-defined network: names go through dockerd's embedded DNS.
	b.dk("network", "rm", "hb-hostgw-net")
	if r := b.dk("network", "create", "hb-hostgw-net"); r.err != nil {
		b.check("create a user-defined network", false, r.String())
	} else {
		r = b.wget([]string{"--network", "hb-hostgw-net"}, "http://host.docker.internal:"+p4+"/")
		b.check("host.docker.internal from a user-defined network", r.err == nil && strings.Contains(r.out, want4), r.String())
	}

	// 3. A server on [::1] only.
	if v6, err := startHTTP("tcp6", "[::1]:0", "hb-hostgw-v6-"+b.token); err != nil {
		fmt.Println("SKIP [::1] fallback:", err)
	} else {
		p6 := strconv.Itoa(v6.port())
		if c, err := net.DialTimeout("tcp4", "127.0.0.1:"+p6, time.Second); err == nil {
			_ = c.Close()
			fmt.Println("SKIP [::1] fallback: 127.0.0.1 has a listener on port", p6)
		} else {
			r = b.wget(nil, "http://host.docker.internal:"+p6+"/")
			b.check("a server on [::1] only is reached (127.0.0.1 refused)", r.err == nil && strings.Contains(r.out, "hb-hostgw-v6-"+b.token), r.String())
		}
		v6.close()
	}

	// 4. UDP.
	if u, err := startUDPEcho("127.0.0.1:0"); err != nil {
		b.check("UDP server", false, err.Error())
	} else {
		r = b.sh(nil, fmt.Sprintf("echo ping-%s | nc -u -w 2 host.docker.internal %d", b.token, u.port()))
		b.check("UDP to host.docker.internal reaches 127.0.0.1", strings.Contains(r.out, "echo ping-"+b.token), r.String())
		u.close()
	}

	// 5. Nothing else through it: other subnet addresses, the host's LAN address.
	r = b.wget(nil, "http://192.168.127.253:"+p4+"/")
	b.check("another guest-subnet address (192.168.127.253) is refused", r.err != nil, r.String())
	lan := hostLANAddr()
	if lan.IsValid() {
		r = b.wget(nil, "http://"+lan.String()+":"+p4+"/")
		b.check("the host's LAN address "+lan.String()+" is refused (the server is on loopback only)", r.err != nil, r.String())
		if b.wildcard {
			wc, err := startHTTP("tcp4", "0.0.0.0:0", "hb-hostgw-wild-"+b.token)
			if err != nil {
				b.check("wildcard server", false, err.Error())
			} else {
				pw := strconv.Itoa(wc.port())
				r = b.wget(nil, "http://"+lan.String()+":"+pw+"/")
				b.check("the host's LAN address stays refused for a server on 0.0.0.0", r.err != nil && !strings.Contains(r.out, "hb-hostgw-wild"), r.String())
				r = b.wget(nil, "http://host.docker.internal:"+pw+"/")
				b.check("host.docker.internal reaches the 0.0.0.0 server through 127.0.0.1", r.err == nil && strings.Contains(r.out, "hb-hostgw-wild-"+b.token), r.String())
				wc.close()
			}
		}
	} else {
		fmt.Println("SKIP host LAN address: none found")
	}

	// 6. Connection flood: 400 held connections from one container.
	if h, err := startHold("127.0.0.1:0"); err != nil {
		b.check("hold server", false, err.Error())
	} else {
		// busybox nc does not exit at the end of its input while the peer keeps the
		// connection open, so timeout ends each one.
		script := fmt.Sprintf("i=0; while [ $i -lt 400 ]; do (sleep 30 | timeout 8 nc host.docker.internal %d >/dev/null 2>&1) & i=$((i+1)); done; wait", h.port())
		done := make(chan result, 1)
		b.dk("rm", "-f", "hb-hostgw-flood")
		go func() { done <- b.sh([]string{"--name", "hb-hostgw-flood"}, script) }()
		time.Sleep(6 * time.Second)
		peak := h.maxOpen.Load()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			b.dk("rm", "-f", "hb-hostgw-flood")
			<-done
		}
		h.close()
		b.check(fmt.Sprintf("400 held connections are capped at 256 (peak %d open at once)", peak), peak > 0 && peak <= 256, "")
		r = b.wget(nil, "http://host.docker.internal:"+p4+"/")
		b.check("the host gateway works after the flood", r.err == nil && strings.Contains(r.out, want4), r.String())
	}

	// 7. Off, live.
	r = b.hbc("config", "set", "hostGateway.enabled", "false")
	b.check("config set hostGateway.enabled false applies to the running engine", r.err == nil && strings.Contains(r.out, "Applied to the running engine"), r.String())
	fmt.Printf("     (config set took %v)\n", r.took.Round(time.Millisecond))
	r = b.sh(nil, "wget -q -T 5 -O - http://"+hgw+":"+p4+"/")
	b.check("off: a connection to "+hgw+" is refused", r.err != nil && !strings.Contains(r.out, want4), r.String())
	r = b.sh(nil, "nslookup host.docker.internal")
	b.check("off: host.docker.internal does not resolve", r.err != nil || !strings.Contains(r.out, hgw), r.String())
	r = b.sh([]string{"--add-host", "foo:host-gateway"}, "wget -q -T 5 -O - http://foo:"+p4+"/")
	b.check("off: --add-host foo:host-gateway is refused", r.err != nil && !strings.Contains(r.out, want4), r.String())
	r = b.hbc("config", "unset", "hostGateway.enabled")
	b.check("config unset hostGateway.enabled applies", r.err == nil && strings.Contains(r.out, "Applied"), r.String())
	r = b.wget(nil, "http://host.docker.internal:"+p4+"/")
	b.check("on again: host.docker.internal reaches the host", r.err == nil && strings.Contains(r.out, want4), r.String())

	// 8. Limited ports.
	r = b.hbc("config", "set", "hostGateway.ports", p4)
	b.check("config set hostGateway.ports "+p4+" applies", r.err == nil && strings.Contains(r.out, "ports "+p4), r.String())
	r = b.wget(nil, "http://host.docker.internal:"+p4+"/")
	b.check("listed port "+p4+" is forwarded", r.err == nil && strings.Contains(r.out, want4), r.String())
	r = b.wget(nil, "http://host.docker.internal:"+po+"/")
	b.check("unlisted port "+po+" is refused", r.err != nil && !strings.Contains(r.out, "hb-hostgw-other"), r.String())
	fmt.Printf("     (refused by name in %v)\n", r.took.Round(time.Millisecond))
	// By address, so a slow DNS answer does not hide a refusal that times out.
	r = b.wget(nil, "http://"+hgw+":"+po+"/")
	b.check(fmt.Sprintf("the refusal is a prompt reset, not a timeout (%v, a fetch took %v)", r.took.Round(time.Millisecond), base.Round(time.Millisecond)),
		r.err != nil && r.took < base+3*time.Second, r.String())
	r = b.hbc("config", "set", "hostGateway.ports", "0")
	b.check("an invalid port list is refused by config set", r.err != nil, r.String())
	r = b.hbc("config", "unset", "hostGateway.ports")
	b.check("config unset hostGateway.ports applies", r.err == nil && strings.Contains(r.out, "ports all"), r.String())
	r = b.wget(nil, "http://host.docker.internal:"+po+"/")
	b.check("every port again: "+po+" is forwarded", r.err == nil && strings.Contains(r.out, "hb-hostgw-other-"+b.token), r.String())

	if st, err := b.status(); err == nil && st.State != nil {
		b.check("no restart: the same VM ran every check", st.State.VMID == vm0, vm0+" -> "+st.State.VMID)
	}
	return nil
}

// hostLANAddr returns a non-loopback, non-link-local IPv4 address of this host.
func hostLANAddr() netip.Addr {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return netip.Addr{}
	}
	for _, a := range as {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if ip = ip.Unmap(); ok && ip.Is4() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip
		}
	}
	return netip.Addr{}
}
