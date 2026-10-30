package vm

import (
	"encoding/json"
	"testing"
)

func TestDocumentPlan9(t *testing.T) {
	c := testConfig(t)
	c.Plan9 = []Plan9Share{{Name: "p9", AccessName: "p9", Path: `C:\share`, Port: 564, ReadOnly: true}}
	doc, err := c.Document()
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		VirtualMachine struct {
			Devices struct {
				Plan9 struct {
					Shares []struct {
						Name, AccessName, Path string
						Port, Flags            uint32
						ReadOnly               bool
					}
				}
			}
		}
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	s := d.VirtualMachine.Devices.Plan9.Shares
	if len(s) != 1 || s[0].Path != `C:\share` || s[0].Port != 564 || s[0].Flags != Plan9FlagReadOnly || !s[0].ReadOnly {
		t.Fatalf("plan9 shares %+v", s)
	}
	// No Plan9 key without shares.
	c.Plan9 = nil
	doc, _ = c.Document()
	var raw map[string]map[string]map[string]any
	json.Unmarshal(doc, &raw)
	if _, ok := raw["VirtualMachine"]["Devices"]["Plan9"]; ok {
		t.Fatal("empty Plan9 rendered")
	}
}

func TestValidatePlan9(t *testing.T) {
	for _, bad := range [][]Plan9Share{
		{{Name: "", Path: `C:\x`, Port: 1}},
		{{Name: "a", Path: `rel`, Port: 1}},
		{{Name: "a", Path: `C:\x`, Port: 0}},
		{{Name: "a", Path: `C:\x`, Port: 1}, {Name: "a", Path: `C:\y`, Port: 2}},
	} {
		c := testConfig(t)
		c.Plan9 = bad
		if err := c.Validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
