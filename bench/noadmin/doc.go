// Command noadmin is the helper of bench/noadmin (run.ps1 drives it; see README.md):
// the checks of ADR 0004's two-account test that PowerShell cannot make itself.
//
//	noadmin record -status status.json -out owner.json
//	noadmin probe -owner owner.json -expect allow|deny
//	noadmin groups
//
// record, run as the owner (a user whose hyperbutler runs through the broker service),
// turns `hyperbutler status --json` into owner.json: the owner's SID, the engine's
// Docker API pipe, its named control events, the VM's runtime id and the owner's
// per-user broker endpoints. Nothing in it is secret: names and ids only.
//
// probe opens each of them: the pipes (CreateFile), the events (OpenEvent with the
// right `hyperbutler stop` needs, never set) and AF_HYPERV connects to the VM on the
// guest's ports. With -expect allow (the owner, a positive control) they must open;
// with -expect deny (a second, standard user) each must be refused with access denied,
// which is told apart from a name that does not exist.
//
// groups checks that the caller is a standard user: not in Administrators, not in
// Hyper-V Administrators, not elevated, at medium integrity (`whoami /groups` and the
// token).
//
// Each subcommand prints one JSON array of checks ({name, ok, detail}) on stdout and
// exits 1 if one failed, 2 on a usage error.
package main
