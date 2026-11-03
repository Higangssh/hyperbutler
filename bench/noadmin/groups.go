package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Well-known SIDs the standard-user check looks for.
const (
	sidAdministrators     = "S-1-5-32-544"
	sidHyperVAdmins       = "S-1-5-32-578"
	sidMediumIntegrity    = "S-1-16-8192"
	sidMandatoryLabelPart = "S-1-16-"
)

// parseWhoamiGroups returns the SIDs of `whoami /groups /fo csv /nh`: one quoted record
// per group, the SID third. The group names and attributes are localized and in the
// console's code page, so only the SID column is read.
func parseWhoamiGroups(r io.Reader) ([]string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	var sids []string
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("whoami /groups: %w", err)
		}
		if len(rec) < 3 {
			continue
		}
		if sid := strings.TrimSpace(rec[2]); strings.HasPrefix(sid, "S-1-") {
			sids = append(sids, sid)
		}
	}
	if len(sids) == 0 {
		return nil, errors.New("whoami /groups: no SID in the output")
	}
	return sids, nil
}

// judgeGroups checks the SIDs of whoami /groups for a standard user: Administrators
// absent (even as a deny-only group, which is an administrator under UAC), Hyper-V
// Administrators absent, and the medium mandatory label.
func judgeGroups(sids []string) []check {
	has := func(want string) bool {
		for _, s := range sids {
			if strings.EqualFold(s, want) {
				return true
			}
		}
		return false
	}
	label := "none"
	for _, s := range sids {
		if strings.HasPrefix(s, sidMandatoryLabelPart) {
			label = s
		}
	}
	return []check{
		{Name: "whoami: not in Administrators", OK: !has(sidAdministrators), Detail: presence(has(sidAdministrators), sidAdministrators)},
		{Name: "whoami: not in Hyper-V Administrators", OK: !has(sidHyperVAdmins), Detail: presence(has(sidHyperVAdmins), sidHyperVAdmins)},
		{Name: "whoami: medium integrity", OK: label == sidMediumIntegrity, Detail: "label " + label},
	}
}

func presence(in bool, sid string) string {
	if in {
		return sid + " is in the token"
	}
	return sid + " is not in the token"
}
