//go:build windows

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"sort"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
	"github.com/Higangssh/hyperbutler/internal/wincert"
)

// guestFile runs cat path in the guest's mount namespace (a privileged container that
// enters PID 1's), and returns what it printed.
func guestFile(dk func(...string) result, image, path string) result {
	return dk("run", "--rm", "--privileged", "--pid=host", image, "nsenter", "-t", "1", "-m", "--", "cat", path)
}

// pemSet is the SHA-256 of the DER of every CERTIFICATE block in b.
func pemSet(b []byte) map[[32]byte]bool {
	m := map[[32]byte]bool{}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return m
		}
		if blk.Type == "CERTIFICATE" {
			m[sha256.Sum256(blk.Bytes)] = true
		}
	}
}

// checkStoreRoot checks the path REQ-UX-009 is about: a root in this machine's
// LocalMachine\Root that the guest's stock Alpine bundle lacks reaches the bundle
// dockerd uses (SSL_CERT_FILE), with ca.import on. It picks such a root itself (one
// that CertSet would import: a CA, valid now, not disallowed), by comparing the store
// with the stock bundle read from the guest. It only reads the store.
func checkStoreRoot(dk, hb func(...string) result, image string) {
	stock := guestFile(dk, image, "/etc/ssl/certs/ca-certificates.crt")
	have := pemSet([]byte(stock.out))
	if stock.code != 0 || len(have) < 50 {
		check("read the guest's stock CA bundle", false, stock.out)
		return
	}
	roots, err := wincert.LocalMachineRoots()
	if err != nil {
		check("read LocalMachine\\Root", false, err.Error())
		return
	}
	var set dockerenv.CertSet
	if err := wincert.LoadDisallowed(&set); err != nil {
		check("read the Untrusted Certificates stores", false, err.Error())
		return
	}
	var cands []string
	for _, der := range roots {
		h := sha256.Sum256(der)
		before := set.Stats.Added
		set.Add(der, true)
		if set.Stats.Added > before && !have[h] {
			cands = append(cands, hex.EncodeToString(h[:]))
		}
	}
	sort.Strings(cands)
	if len(cands) == 0 {
		check("a LocalMachine\\Root certificate that the stock bundle lacks", false, fmt.Sprintf("%d roots, all in the stock bundle or not importable", len(roots)))
		return
	}
	pick := cands[0]
	fmt.Printf("     %d of %d LocalMachine\\Root certificates are importable and not in the stock bundle (%d entries); checking %s\n",
		len(cands), len(roots), len(have), pick)
	bundle := guestFile(dk, image, "/run/hyperbutler/ca-certificates.crt")
	got := pemSet([]byte(bundle.out))
	var want [32]byte
	_, _ = hex.Decode(want[:], []byte(pick))
	check("guest: the combined bundle has the LocalMachine\\Root certificate "+pick[:16], bundle.code == 0 && got[want], fmt.Sprintf("exit %d, %d certificates", bundle.code, len(got)))
	missing := 0
	for _, c := range cands {
		var h [32]byte
		_, _ = hex.Decode(h[:], []byte(c))
		if !got[h] {
			missing++
		}
	}
	check("guest: the combined bundle has every importable LocalMachine\\Root certificate", missing == 0, fmt.Sprintf("%d of %d missing", missing, len(cands)))

	st := hb("status", "--json")
	var s struct {
		State struct {
			DaemonConfig struct {
				Env struct {
					CAImport bool `json:"caImport"`
					CAStats  struct {
						Added int `json:"added"`
					} `json:"caStats"`
				} `json:"env"`
			} `json:"daemonConfig"`
		} `json:"state"`
	}
	err = json.NewDecoder(bytes.NewReader([]byte(st.out))).Decode(&s)
	e := s.State.DaemonConfig.Env
	check("status --json: caImport on and state.daemonConfig.env.caStats.added > 0", err == nil && e.CAImport && e.CAStats.Added > 0, st.out)
}
