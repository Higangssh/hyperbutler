package engine

import "testing"

// TestUpdateChecked: the user's setting wins; without one, the machine's default decides.
func TestUpdateChecked(t *testing.T) {
	saved := UpdateCheckDefault
	t.Cleanup(func() { UpdateCheckDefault = saved })
	yes, no := true, false
	for _, machine := range []bool{true, false} {
		UpdateCheckDefault = func() bool { return machine }
		if got := (&Settings{}).UpdateChecked(); got != machine {
			t.Errorf("machine %v, no setting: %v", machine, got)
		}
		if !(&Settings{UpdateCheck: &yes}).UpdateChecked() || (&Settings{UpdateCheck: &no}).UpdateChecked() {
			t.Errorf("machine %v: the user's setting did not win", machine)
		}
	}
}
