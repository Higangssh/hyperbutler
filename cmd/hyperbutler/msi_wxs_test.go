package main

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wxsCustom is one <Custom> of the MSI's InstallExecuteSequence, with its <CustomAction>.
type wxsCustom struct {
	condition, execute, ret, impersonate string
}

// readWXS reads dist/msi/hyperbutler.wxs: every scheduled custom action, the MajorUpgrade
// schedule, and the elements that appear only inside <?if $(var.FailForTest) = 1 ?>.
func readWXS(t *testing.T) (map[string]*wxsCustom, string, map[string]bool) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "dist", "msi", "hyperbutler.wxs"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := xml.NewDecoder(f)
	actions := map[string]*wxsCustom{}
	get := func(id string) *wxsCustom {
		if actions[id] == nil {
			actions[id] = &wxsCustom{}
		}
		return actions[id]
	}
	schedule := ""
	testOnly := map[string]bool{} // element name: seen only inside the FailForTest block
	var block []string            // the preprocessor blocks around the token: "test", "else", "other"
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		switch tok := tok.(type) {
		case xml.ProcInst:
			inst := strings.TrimSpace(tok.Target + " " + string(tok.Inst))
			switch {
			case strings.HasPrefix(inst, "if "):
				if strings.Contains(inst, "$(var.FailForTest) = 1") {
					block = append(block, "test")
				} else {
					block = append(block, "other")
				}
			case inst == "else" && len(block) > 0:
				block[len(block)-1] = "else"
			case inst == "endif" && len(block) > 0:
				block = block[:len(block)-1]
			}
		case xml.StartElement:
			attr := func(n string) string {
				for _, a := range tok.Attr {
					if a.Name.Local == n {
						return a.Value
					}
				}
				return ""
			}
			inTest := len(block) > 0 && block[len(block)-1] == "test"
			switch tok.Name.Local {
			case "Custom":
				get(attr("Action")).condition = attr("Condition")
			case "CustomAction":
				c := get(attr("Id"))
				c.execute, c.ret, c.impersonate = attr("Execute"), attr("Return"), attr("Impersonate")
			case "MajorUpgrade":
				schedule = attr("Schedule")
			case "Launch", "FailWhenDeferred":
				if seen, ok := testOnly[tok.Name.Local]; !ok || seen {
					testOnly[tok.Name.Local] = inTest
				}
			}
		}
	}
	return actions, schedule, testOnly
}

// requires reports whether the Windows Installer condition cond holds only when clause
// does: cond is a conjunction (no OR outside parentheses) with clause as one of its
// top-level terms. A substring is not enough: "NOT Installed OR X" runs on a repair.
func requires(cond, clause string) bool {
	var terms []string
	depth, start := 0, 0
	for i := 0; i < len(cond); i++ {
		switch {
		case cond[i] == '(':
			depth++
		case cond[i] == ')':
			depth--
		case depth == 0 && strings.HasPrefix(cond[i:], " OR "):
			return false
		case depth == 0 && strings.HasPrefix(cond[i:], " AND "):
			terms = append(terms, strings.TrimSpace(cond[start:i]))
			start = i + len(" AND ")
		}
	}
	terms = append(terms, strings.TrimSpace(cond[start:]))
	for _, t := range terms {
		if t == clause {
			return true
		}
	}
	return false
}

func TestRequires(t *testing.T) {
	for cond, want := range map[string]bool{
		`NOT Installed`:                        true,
		`NOT Installed AND ADDHYPERVADMIN="1"`: true,
		`X AND (A OR B) AND NOT Installed`:     true,
		`NOT Installed OR X`:                   false,
		`X OR NOT Installed AND Y`:             false,
		`(NOT Installed OR X) AND Y`:           false,
		`NOT InstalledFoo AND X`:               false,
		``:                                     false,
	} {
		if got := requires(cond, "NOT Installed"); got != want {
			t.Errorf("requires(%q) = %v", cond, got)
		}
	}
}

// TestMSISequence holds the installer's custom actions to the rules of ADR 0004,
// Installer (M4): the one SYSTEM step a repair (which a standard user can start) runs is
// the idempotent broker install; taking accounts out of Hyper-V Administrators needs a
// real upgrade; a first install fails when the broker does not install, an upgrade or a
// repair does not; the old product's removal is inside the transaction; the failing test
// hooks exist only in the -FailForTest build.
func TestMSISequence(t *testing.T) {
	actions, schedule, testOnly := readWXS(t)
	if schedule != "afterInstallInitialize" {
		t.Errorf("MajorUpgrade Schedule %q", schedule)
	}
	for id, c := range actions {
		if c.execute != "deferred" || c.impersonate != "no" {
			continue // immediate or rollback, or not a SYSTEM step
		}
		cond := c.condition
		repairRuns := !requires(cond, "NOT Installed") && !requires(cond, `REMOVE~="ALL"`)
		if repairRuns && id != "HbBrokerInstall" {
			t.Errorf("%s runs as SYSTEM on a repair (condition %q)", id, cond)
		}
	}
	want := map[string]struct{ cond, ret string }{
		"HbBrokerInstallFirst": {`NOT Installed AND NOT WIX_UPGRADE_DETECTED`, "check"},
		"HbBrokerInstall":      {`NOT REMOVE~="ALL" AND (Installed OR WIX_UPGRADE_DETECTED)`, "ignore"},
		"HbDropHyperVAdmins":   {`NOT Installed AND WIX_UPGRADE_DETECTED AND NOT REMOVE~="ALL" AND NOT ADDHYPERVADMIN="1"`, "ignore"},
		"HbBrokerUninstall":    {`REMOVE~="ALL"`, "ignore"},
	}
	for id, w := range want {
		c := actions[id]
		if c == nil || c.condition != w.cond || c.ret != w.ret || c.execute != "deferred" || c.impersonate != "no" {
			t.Errorf("%s: %+v, want condition %q, Return %q, deferred as SYSTEM", id, c, w.cond, w.ret)
		}
	}
	for _, id := range []string{"HbBrokerInstallRollback", "HbBrokerUninstallRollback"} {
		if c := actions[id]; c == nil || c.execute != "rollback" || c.condition == "" {
			t.Errorf("%s: %+v", id, c)
		}
	}
	for _, el := range []string{"Launch", "FailWhenDeferred"} {
		if inTest, ok := testOnly[el]; !ok || !inTest {
			t.Errorf("<%s> is not only in the FailForTest build (%v, %v)", el, inTest, ok)
		}
	}
}

// TestMSINoRestartManager: an upgrade must not ask the user to close the broker service,
// which Restart Manager finds running at InstallValidate (the old product's removal stops
// it before its files go).
func TestMSINoRestartManager(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "dist", "msi", "hyperbutler.wxs"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := xml.NewDecoder(f)
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if el, ok := tok.(xml.StartElement); ok && el.Name.Local == "Property" {
			var id, value string
			for _, a := range el.Attr {
				switch a.Name.Local {
				case "Id":
					id = a.Value
				case "Value":
					value = a.Value
				}
			}
			if id == "MSIRESTARTMANAGERCONTROL" {
				if value != "Disable" {
					t.Errorf("MSIRESTARTMANAGERCONTROL is %q, want Disable", value)
				}
				return
			}
		}
	}
	t.Error("hyperbutler.wxs does not set MSIRESTARTMANAGERCONTROL")
}
