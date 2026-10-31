package engine

// hashCapMB is the guest size whose default dentry and inode hash tables
// HashTableArgs keeps for larger guests.
const hashCapMB = 4096

// HashTableArgs returns the kernel arguments that cap the dentry and inode hash tables
// of a guest with memoryMB of RAM ("" or " dhash_entries=... ihash_entries=...").
//
// The kernel sizes both tables from RAM (one dentry bucket per 2 pages, one inode
// bucket per 4, rounded up to a power of two), zeroes them at boot, and so they stay
// committed on the host: 16 + 8 MiB in a 12 GiB guest. Above 4 GiB they are capped at
// the sizes a 4 GiB guest gets by default, 2^19 and 2^18 buckets (4 + 2 MiB); at 4 GiB
// and below the kernel's own sizes stay. See docs/research/mem-reclaim.md (REQ-MEM-002).
func HashTableArgs(memoryMB int) string {
	if memoryMB <= hashCapMB {
		return ""
	}
	return " dhash_entries=524288 ihash_entries=262144"
}
