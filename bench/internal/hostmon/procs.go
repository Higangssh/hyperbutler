package hostmon

import (
	"fmt"
	"sort"
	"strings"
)

// Proc is one entry of a process snapshot.
type Proc struct {
	PID, PPID uint32
	Name      string // image name, e.g. "vmmem" or "hyperbutler.exe"
}

// Image names of the processes the benches care about.
const (
	VMMemName  = "vmmem"    // the VM's memory process (HCS); child of its vmwp
	VMWPName   = "vmwp.exe" // the VM worker process
	EngineName = "hyperbutler.exe"
	DevName    = "hyperbutler-dev.exe"
	BrokerName = "hyperbutler-broker.exe" // the HCS access broker service (ADR 0004)
)

func isName(p Proc, name string) bool { return strings.EqualFold(p.Name, name) }

// isVMMem matches every VM memory process: HCS's "vmmem" and its variants such as
// WSL's "vmmemWSL" or Windows Sandbox's "vmmemCmZygote".
func isVMMem(p Proc) bool {
	return len(p.Name) >= len(VMMemName) && strings.EqualFold(p.Name[:len(VMMemName)], VMMemName)
}

// StillOurs reports whether vm's processes are still in ps as the pair FindNewVM
// found: the vmmem PID still a vmmem whose parent is the vmwp PID. vmmem cannot be
// opened (pinned) by a non-elevated user, so this is how a bench notices that its VM
// died and its PID was reused.
func StillOurs(ps []Proc, vm VMProcs) bool {
	mem, wp := false, false
	for _, p := range ps {
		switch p.PID {
		case vm.VMMem:
			mem = isName(p, VMMemName) && p.PPID == vm.VMWP
		case vm.VMWP:
			wp = isName(p, VMWPName)
		}
	}
	return mem && wp
}

// VMProcs are one VM's host processes.
type VMProcs struct {
	VMWP, VMMem uint32
}

// FindNewVM picks the VM that appeared between two snapshots: the vmmem process that is
// in after but not in before, whose parent is a vmwp that is also new. A non-elevated
// user cannot open either process to read its VM id, so "new since the start command
// ran" is how a bench tells its VM from others. It is an error unless exactly one such
// pair exists: zero means the VM did not start (or started before before was taken),
// two or more means another VM started at the same time and the result would be a
// guess.
func FindNewVM(before, after []Proc) (VMProcs, error) {
	old := make(map[uint32]bool, len(before))
	for _, p := range before {
		old[p.PID] = true
	}
	newWP := map[uint32]bool{}
	for _, p := range after {
		if isName(p, VMWPName) && !old[p.PID] {
			newWP[p.PID] = true
		}
	}
	var found []VMProcs
	for _, p := range after {
		if isName(p, VMMemName) && !old[p.PID] && newWP[p.PPID] {
			found = append(found, VMProcs{VMWP: p.PPID, VMMem: p.PID})
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return VMProcs{}, fmt.Errorf("hostmon: no new vmmem with a new vmwp parent (%d new vmwp)", len(newWP))
	default:
		return VMProcs{}, fmt.Errorf("hostmon: %d VMs started at the same time; cannot tell which is ours", len(found))
	}
}

// Foreign counts the processes in ps that belong to other VMs or engines: vmmem and
// vmwp processes other than ours, and hyperbutler engine or harness processes other
// than ours (the bench's own hyperbutler CLI calls are short-lived children and are
// listed in ours by the caller when they matter). ours may be nil.
type Foreign struct {
	VMs     []uint32 // other vmmem PIDs
	Workers []uint32 // other vmwp PIDs
	Engines []uint32 // other hyperbutler.exe / hyperbutler-dev.exe PIDs
}

// Any reports whether anything foreign is running.
func (f Foreign) Any() bool { return len(f.VMs)+len(f.Workers)+len(f.Engines) > 0 }

func (f Foreign) String() string {
	if !f.Any() {
		return "none"
	}
	return fmt.Sprintf("vmmem %v, vmwp %v, engines %v", f.VMs, f.Workers, f.Engines)
}

// FindForeign classifies ps against ours (the PIDs that belong to the bench's own
// engine and VM, and any short-lived CLI processes it started).
func FindForeign(ps []Proc, ours map[uint32]bool) Foreign {
	var f Foreign
	for _, p := range ps {
		if ours[p.PID] {
			continue
		}
		switch {
		case isVMMem(p):
			f.VMs = append(f.VMs, p.PID)
		case isName(p, VMWPName):
			f.Workers = append(f.Workers, p.PID)
		case isName(p, EngineName), isName(p, DevName):
			f.Engines = append(f.Engines, p.PID)
		}
	}
	for _, s := range [][]uint32{f.VMs, f.Workers, f.Engines} {
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	}
	return f
}

// Descendants returns root and every process in ps below it (children, their
// children, ...). A bench uses it to tell its own short-lived hyperbutler.exe CLI and
// engine processes from another user's. A PPID can name a parent that exited and whose
// PID was reused, so this over-approximates only through such reuse.
func Descendants(ps []Proc, root uint32) map[uint32]bool {
	kids := map[uint32][]uint32{}
	for _, p := range ps {
		if p.PID != p.PPID {
			kids[p.PPID] = append(kids[p.PPID], p.PID)
		}
	}
	out := map[uint32]bool{root: true}
	queue := []uint32{root}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, k := range kids[n] {
			if !out[k] {
				out[k] = true
				queue = append(queue, k)
			}
		}
	}
	return out
}

// ChildrenOf returns the PIDs in ps whose parent is pid (for example helper processes
// of the engine, which REQ-MEM-001 counts as host memory).
func ChildrenOf(ps []Proc, pid uint32) []uint32 {
	var c []uint32
	for _, p := range ps {
		if p.PPID == pid && p.PID != pid {
			c = append(c, p.PID)
		}
	}
	return c
}

// ByName returns the PIDs of the processes with this image name.
func ByName(ps []Proc, name string) []uint32 {
	var pids []uint32
	for _, p := range ps {
		if isName(p, name) {
			pids = append(pids, p.PID)
		}
	}
	return pids
}
