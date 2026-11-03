// Package hvaddr holds the address forms of hyperbutler's Hyper-V sockets and named
// pipes (service IDs, targets, pipe names) with no I/O and only the standard library and
// hyperbutler's own leaf packages: what the broker service needs of them, as rule 10 of
// ADR 0004 allows it nothing else. internal/transport does the I/O and re-exports these.
package hvaddr
