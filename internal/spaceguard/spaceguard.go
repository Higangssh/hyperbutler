// Package spaceguard decides how much guest file-system space winbox holds back so that
// the guest runs out of space (ENOSPC) before the host volume that holds its sparse VHDX
// does (REQ-DISK-004).
//
// A sparse VHDX grows on the host as the guest writes to blocks the VHDX does not back yet.
// When the host volume fills, HCS does not pause the VM: the guest gets EIO and ext4
// aborts its journal, losing in-flight data. The guard prevents that by keeping an
// fallocate'd reservation file in the guest. fallocate takes guest blocks without writing
// them, so the VHDX does not grow, and the guest has fewer blocks it can write.
//
// # Invariant
//
// Let D be the guest block device size, R the reservation, A the VHDX's host allocation,
// M the part of A that is VHDX metadata, and F_h the host volume's free space. A guest
// that writes every block outside the reservation grows the VHDX by at most
//
//	exposure = D - R - (A - M)
//
// because A - M bytes of the device are already backed. The guard keeps
//
//	exposure <= F_h - Margin.
//
// This counts everything that can grow the VHDX: free blocks, dirty data not yet
// written, ext4 metadata that mke2fs wrote as zeros (vhdmp leaves zero-written blocks
// unallocated), and files written as zeros and overwritten later. It needs no guess about
// which used blocks are backed.
//
// When the guest writes W new bytes, A grows by W and F_h shrinks by W, so both sides of
// the inequality move together. A budget measured before a guest write is still exact
// after it: the guest's own write rate does not matter, however bursty. Only other host
// writers consume F_h behind the guard's back; the margin covers one update interval of
// those, and the hard floor (PauseWanted) is the last resort.
//
// The estimate is optimistic only where the reservation sits on blocks that are already
// backed (fallocate picked deleted blocks that were not trimmed yet) and where a 1 MiB
// VHDX block is shared between reservation and data. Trimming before growing the
// reservation bounds the first; the margin covers the second.
//
// # Split
//
// The host computes a Budget from its measurements (HostBudget): F_h - Margin and A - M.
// The guest computes the reservation from its own device size and the budget
// (GuestReservation). Plan runs both for tests and callers that have all inputs.
//
// Everything here is pure arithmetic on uint64 with explicit overflow checks. It has no
// platform code and runs on every GOOS. See docs/research/disk-reclaim.md,
// "Free-space guard".
package spaceguard

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// Config holds the guard's tunables. All values are bytes.
type Config struct {
	// Margin is how much host free space the guest must never be able to consume. It
	// must cover host writes by other processes during one update interval and VHDX
	// block-sharing slack.
	Margin uint64
	// Floor is the hard floor: below this much host free space the VM is paused. It must
	// be less than Margin, so that a working guard hits ENOSPC in the guest first.
	Floor uint64
	// ResumeGap is how far above Floor host free space must climb before a paused VM is
	// resumed.
	ResumeGap uint64
	// Hysteresis is the width of the band the reservation may sit in without being
	// changed. Growing overshoots by half of it; shrinking waits until the reservation is
	// a full Hysteresis above what is needed.
	Hysteresis uint64
	// Quantum is the reservation granularity. Targets are rounded up to a multiple of it.
	Quantum uint64
}

// DefaultConfig is a starting point for a real host volume. The spike uses smaller
// values to fit a 3 GiB test volume.
var DefaultConfig = Config{
	Margin:     1 << 30,   // 1 GiB
	Floor:      256 << 20, // 256 MiB
	ResumeGap:  256 << 20,
	Hysteresis: 64 << 20,
	Quantum:    1 << 20, // the winbox VHDX block size
}

// Validate reports a configuration the policy cannot honour.
func (c Config) Validate() error {
	switch {
	case c.Quantum == 0:
		return errors.New("spaceguard: quantum must be > 0")
	case c.Margin == 0:
		return errors.New("spaceguard: margin must be > 0")
	case c.Floor >= c.Margin:
		return fmt.Errorf("spaceguard: floor %d must be below margin %d, or the VM pauses before the guest sees ENOSPC", c.Floor, c.Margin)
	}
	return nil
}

// Host is what the host measures.
type Host struct {
	// Free is the free space of the host volume that holds the VHDX, as available to the
	// VM worker (GetDiskFreeSpaceEx's caller-available bytes, which honours quotas).
	Free uint64
	// VHDXAllocated is the bytes the VHDX occupies on the host volume now.
	VHDXAllocated uint64
	// VHDXMetadata is the part of VHDXAllocated that is VHDX headers, log, BAT and
	// metadata rather than guest data: the allocation of the empty VHDX right after it
	// was created. Overstating it is safe; understating it is not.
	VHDXMetadata uint64
	// VHDXMax is the largest the VHDX file can become (virtual size plus metadata). 0
	// means unknown, and the guard then never treats the guest as unlimited.
	VHDXMax uint64
}

// Budget is the host's message to the guest.
type Budget struct {
	// Bytes is how many more host bytes the VHDX may grow by: host free minus margin.
	Bytes uint64
	// Backed is how many bytes of the guest device the VHDX already backs (A - M).
	Backed uint64
	// Unlimited means the VHDX cannot grow by more than the host can spare, so the guest
	// needs no reservation. Bytes and Backed are then zero.
	Unlimited bool
}

func (b Budget) String() string {
	if b.Unlimited {
		return "unlimited"
	}
	return fmt.Sprintf("%d %d", b.Bytes, b.Backed)
}

// HostBudget computes the budget from host measurements.
func HostBudget(h Host, c Config) (Budget, error) {
	if err := c.Validate(); err != nil {
		return Budget{}, err
	}
	if h.VHDXMax != 0 && h.VHDXAllocated > h.VHDXMax {
		return Budget{}, fmt.Errorf("spaceguard: VHDX allocation %d exceeds its maximum %d; the maximum is wrong", h.VHDXAllocated, h.VHDXMax)
	}
	if h.VHDXMetadata > h.VHDXAllocated {
		return Budget{}, fmt.Errorf("spaceguard: VHDX metadata %d exceeds its allocation %d", h.VHDXMetadata, h.VHDXAllocated)
	}
	spare := satSub(h.Free, c.Margin)
	if h.VHDXMax != 0 && h.VHDXMax-h.VHDXAllocated <= spare {
		return Budget{Unlimited: true}, nil
	}
	return Budget{Bytes: spare, Backed: h.VHDXAllocated - h.VHDXMetadata}, nil
}

// Guest is what the guest measures.
type Guest struct {
	// Device is the size of the block device that holds the file system (BLKGETSIZE64).
	Device uint64
	// Size is the file system size (statfs f_blocks * f_frsize). It excludes metadata
	// such as the ext4 journal and inode tables, so it is at most Device.
	Size uint64
	// Used is (f_blocks - f_bfree) * f_frsize. It includes the reservation.
	Used uint64
	// Free is f_bfree * f_frsize: all free blocks, including those reserved for root.
	Free uint64
	// Reserved is the space the reservation file holds now (st_blocks * 512).
	Reserved uint64
}

// Validate reports guest measurements that cannot all be true at once.
func (g Guest) Validate() error {
	switch {
	case g.Size == 0:
		return errors.New("spaceguard: guest file system size is 0")
	case g.Device < g.Size:
		return fmt.Errorf("spaceguard: device size %d is below file system size %d", g.Device, g.Size)
	case g.Used > g.Size || g.Free > g.Size-g.Used: // written so that it cannot overflow
		return fmt.Errorf("spaceguard: guest used %d + free %d exceeds size %d", g.Used, g.Free, g.Size)
	case g.Reserved > g.Used:
		return fmt.Errorf("spaceguard: reservation %d exceeds used space %d", g.Reserved, g.Used)
	}
	return nil
}

// Reservation is the guest's decision.
type Reservation struct {
	// Target is the size to give the reservation file.
	Target uint64
	// Need is the smallest reservation that keeps the exposure within the budget,
	// capped at what the guest could reserve at all (Free + Reserved).
	Need uint64
	// Short is how far the exposure stays over the budget even with every free block
	// reserved. Non-zero means the guard alone cannot protect the host: blocks that are
	// already used but not backed (dirty data, zero-written files, unwritten metadata)
	// can still grow the VHDX past the margin. The host's hard floor is then the only
	// protection.
	Short uint64
}

// GuestReservation computes the reservation file size from guest measurements and the
// host's budget. Reserved in g is the current reservation, used for hysteresis.
func GuestReservation(g Guest, b Budget, c Config) (Reservation, error) {
	if err := c.Validate(); err != nil {
		return Reservation{}, err
	}
	if err := g.Validate(); err != nil {
		return Reservation{}, err
	}
	// The space the guest could reserve. It cannot overflow:
	// Free + Reserved <= Free + Used <= Size.
	unreserved := g.Free + g.Reserved

	var need, short uint64
	if !b.Unlimited {
		// exposure(R) = Device - R - Backed <= Bytes  =>  R >= Device - Backed - Bytes.
		need = satSub(satSub(g.Device, b.Backed), b.Bytes)
		if need > unreserved {
			short = need - unreserved
			need = unreserved
		}
	}

	// Hysteresis band [need, hi]. Inside it, leave the file alone. Outside it, move to the
	// set point. Both ends are rounded with the same function, so the set point is never
	// above hi and applying a decision twice gives the same answer.
	hi := roundUpCap(satAdd(need, c.Hysteresis), c.Quantum, unreserved)
	set := roundUpCap(satAdd(need, c.Hysteresis/2), c.Quantum, unreserved)
	if need == 0 {
		set = 0 // nothing is needed: release everything, don't keep half a band.
	}
	target := g.Reserved
	if g.Reserved < need || g.Reserved > hi {
		target = set
	}
	return Reservation{Target: target, Need: need, Short: short}, nil
}

// PauseWanted is the last resort: pause the VM when host free space is below the floor,
// and keep it paused until free space is ResumeGap above the floor.
func PauseWanted(hostFree uint64, paused bool, c Config) bool {
	if hostFree < c.Floor {
		return true
	}
	return paused && hostFree < satAdd(c.Floor, c.ResumeGap)
}

// Input is everything Plan needs.
type Input struct {
	Host   Host
	Guest  Guest
	Paused bool // the VM is paused by the guard now
}

// Decision is Plan's output.
type Decision struct {
	Budget      Budget
	Reservation Reservation
	Pause       bool
}

// Plan runs both halves of the policy on one set of measurements.
func Plan(in Input, c Config) (Decision, error) {
	b, err := HostBudget(in.Host, c)
	if err != nil {
		return Decision{}, err
	}
	r, err := GuestReservation(in.Guest, b, c)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Budget: b, Reservation: r, Pause: PauseWanted(in.Host.Free, in.Paused, c)}, nil
}

func satSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func satAdd(a, b uint64) uint64 {
	s, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return math.MaxUint64
	}
	return s
}

// roundUpCap rounds x up to a multiple of q, but never above limit. x <= limit is not
// required: the result is min(roundUp(x, q), limit).
func roundUpCap(x, q, limit uint64) uint64 {
	if x >= limit {
		return limit
	}
	r := x % q
	if r == 0 {
		return x
	}
	up := q - r
	if up > limit-x {
		return limit
	}
	return x + up
}
