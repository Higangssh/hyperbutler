package vm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDocumentDisksAndMemoryHints(t *testing.T) {
	c := testConfig(t)
	c.MemoryHints = true
	c.Disks = []Disk{{Path: `C:\d\data.vhdx`, Sparse: true}, {Path: `C:\d\ro.vhdx`, ReadOnly: true}}
	b, err := c.Document()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		VirtualMachine struct {
			ComputeTopology struct {
				Memory map[string]any
			}
			Devices struct {
				Scsi map[string]struct {
					Attachments map[string]map[string]any
				}
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	m := doc.VirtualMachine.ComputeTopology.Memory
	for _, k := range []string{"AllowOvercommit", "EnableDeferredCommit", "EnableHotHint", "EnableColdDiscardHint"} {
		if m[k] != true {
			t.Errorf("Memory.%s = %v, want true", k, m[k])
		}
	}
	att := doc.VirtualMachine.Devices.Scsi["0"].Attachments
	if len(att) != 2 {
		t.Fatalf("attachments = %v", att)
	}
	if att["0"]["Type"] != "VirtualDisk" || att["0"]["Path"] != `C:\d\data.vhdx` || att["0"]["AlwaysAllowSparseFiles"] != true || att["0"]["ReadOnly"] != nil {
		t.Errorf("LUN 0 = %v", att["0"])
	}
	if att["1"]["ReadOnly"] != true || att["1"]["AlwaysAllowSparseFiles"] != nil {
		t.Errorf("LUN 1 = %v", att["1"])
	}
}

func TestDocumentNoHintsNoDisks(t *testing.T) {
	b, err := testConfig(t).Document()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Scsi", "EnableHotHint", "EnableDeferredCommit", "EnableColdDiscardHint"} {
		if strings.Contains(string(b), s) {
			t.Errorf("document has %s without asking: %s", s, b)
		}
	}
}

func TestValidateDisks(t *testing.T) {
	for _, d := range [][]Disk{
		{{Path: `relative.vhdx`}},
		{{Path: `C:\a.vhdx`}, {Path: `c:\A.VHDX`}},
	} {
		c := testConfig(t)
		c.Disks = d
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%v) = nil", d)
		}
	}
}

func TestParseSummariesAndState(t *testing.T) {
	l, err := parseSummaries(`[{"Id":"hyperbutler-1","Owner":"hyperbutler","State":"Running","RuntimeId":"x"}]`)
	if err != nil || len(l) != 1 || l[0].ID != "hyperbutler-1" || l[0].Owner != "hyperbutler" || l[0].State != "Running" {
		t.Fatalf("parseSummaries = %v, %v", l, err)
	}
	if l, err := parseSummaries("null"); err != nil || l != nil {
		t.Fatalf("null: %v, %v", l, err)
	}
	if _, err := parseSummaries(`{"Id":1}`); err == nil {
		t.Fatal("accepted an object")
	}
	if s, err := parseState(`{"State":"Paused"}`); err != nil || s != "Paused" {
		t.Fatalf("parseState = %q, %v", s, err)
	}
	if _, err := parseState(`{}`); err == nil {
		t.Fatal("accepted a document without State")
	}
}

// FuzzParseSummaries: never panics, stays bounded, and accepts only JSON arrays (or an
// empty or null document).
func FuzzParseSummaries(f *testing.F) {
	f.Add(`[{"Id":"hyperbutler-1","Owner":"hyperbutler","State":"Running"}]`)
	f.Add(`null`)
	f.Add(``)
	f.Add(`[{"Id":null},{}]`)
	f.Fuzz(func(t *testing.T, doc string) {
		l, err := parseSummaries(doc)
		if err != nil {
			return
		}
		if len(l) > maxSummaries {
			t.Fatalf("%d summaries", len(l))
		}
		if len(l) > 0 && !strings.HasPrefix(strings.TrimLeft(doc, " \t\r\n"), "[") {
			t.Fatalf("accepted a non-array %q", doc)
		}
	})
}

// FuzzParseState: an accepted state is never empty.
func FuzzParseState(f *testing.F) {
	f.Add(`{"State":"Running"}`)
	f.Add(`{"State":""}`)
	f.Fuzz(func(t *testing.T, doc string) {
		s, err := parseState(doc)
		if err == nil && s == "" {
			t.Fatal("empty state accepted")
		}
	})
}
