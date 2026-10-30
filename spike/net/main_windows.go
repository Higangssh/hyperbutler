// Command net-spike answers one question for ADR 0006: how does an HCS guest get outbound
// network access on Windows Home without elevation?
//
// Subcommands:
//
//	list                 print every HNS network and endpoint (read-only)
//	hns                  create a winbox- NAT network and endpoint, boot an HCS VM with it
//	                     attached, run the guest probe, then delete what it created
//	relay                boot an HCS VM with no NIC and run the guest probe through a
//	                     host-side CONNECT relay over hvsock (user-mode path)
//	cleanup              delete the objects recorded in the state file
//
// Every object it creates is recorded in a state file before use, and cleanup deletes only
// IDs from that file whose names start with "winbox-". Throwaway code; see
// docs/research/guest-networking.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Higangssh/winbox/internal/hcn"
)

var workDir = filepath.Join(os.TempDir(), "hb-dev", "winbox-spike", "net")

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: net-spike list|hns|relay|cleanup [flags]")
		os.Exit(2)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal(err)
	}
	var err error
	switch os.Args[1] {
	case "list":
		err = runList()
	case "hns":
		err = runHNS(os.Args[2:])
	case "relay":
		err = runRelay(os.Args[2:])
	case "baseline":
		_, err = runBaseline()
	case "cleanup":
		err = runCleanup()
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func runList() error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	raw := fs.Bool("raw", false, "print full property documents")
	fs.Parse(os.Args[2:])
	if err := hcn.Load(); err != nil {
		return fmt.Errorf("load computenetwork.dll: %w", err)
	}
	nets, err := hcn.EnumerateNetworks("")
	if err != nil {
		return err
	}
	fmt.Printf("%d network(s)\n", len(nets))
	for _, id := range nets {
		doc, err := hcn.NetworkProperties(id)
		if err != nil {
			fmt.Printf("  %s: %v\n", id, err)
			continue
		}
		n, err := hcn.ParseNetwork(doc)
		if err != nil {
			fmt.Printf("  %s: %v\n", id, err)
			continue
		}
		var prefixes []string
		for _, ip := range n.Ipams {
			for _, s := range ip.Subnets {
				prefixes = append(prefixes, s.IPAddressPrefix)
			}
		}
		fmt.Printf("  %s name=%q type=%s flags=0x%x subnets=%v\n", id, n.Name, n.Type, n.Flags, prefixes)
		if *raw {
			fmt.Println("    " + doc)
		}
	}
	eps, err := hcn.EnumerateEndpoints("")
	if err != nil {
		return err
	}
	fmt.Printf("%d endpoint(s)\n", len(eps))
	for _, id := range eps {
		doc, err := hcn.EndpointProperties(id)
		if err != nil {
			fmt.Printf("  %s: %v\n", id, err)
			continue
		}
		e, err := hcn.ParseEndpoint(doc)
		if err != nil {
			fmt.Printf("  %s: %v\n", id, err)
			continue
		}
		fmt.Printf("  %s name=%q network=%s ips=%v\n", id, e.Name, e.HostComputeNetwork, e.IPConfigurations)
	}
	return nil
}

// state records what this spike created, so cleanup never touches anything else.
type state struct {
	Networks  []string `json:"networks"`
	Endpoints []string `json:"endpoints"`
}

func statePath() string { return filepath.Join(workDir, "created.json") }

func loadState() (state, error) {
	var s state
	b, err := os.ReadFile(statePath())
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func saveState(s state) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(), b, 0o644)
}

func record(kind, id string) error {
	s, err := loadState()
	if err != nil {
		return err
	}
	if kind == "network" {
		s.Networks = append(s.Networks, id)
	} else {
		s.Endpoints = append(s.Endpoints, id)
	}
	return saveState(s)
}

// forget drops id from the state file, for a create that failed.
func forget(id string) {
	s, err := loadState()
	if err != nil {
		return
	}
	drop := func(ids []string) []string {
		var out []string
		for _, x := range ids {
			if x != id {
				out = append(out, x)
			}
		}
		return out
	}
	s.Networks, s.Endpoints = drop(s.Networks), drop(s.Endpoints)
	_ = saveState(s)
}

func runCleanup() error {
	if err := hcn.Load(); err != nil {
		return err
	}
	s, err := loadState()
	if err != nil {
		return err
	}
	var left state
	for _, id := range s.Endpoints {
		if err := hcn.DeleteEndpoint(id); err != nil {
			fmt.Printf("endpoint %s: %v\n", id, err)
			left.Endpoints = append(left.Endpoints, id)
		} else {
			fmt.Printf("endpoint %s deleted\n", id)
		}
	}
	for _, id := range s.Networks {
		if err := hcn.DeleteNetwork(id); err != nil {
			fmt.Printf("network %s: %v\n", id, err)
			left.Networks = append(left.Networks, id)
		} else {
			fmt.Printf("network %s deleted\n", id)
		}
	}
	return saveState(left)
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000) }
