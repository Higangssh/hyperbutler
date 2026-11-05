//go:build !windows

package apply

import "errors"

func applyPlanned(string, []Change, [][]string, []error) ([]Outcome, error) {
	return nil, errors.New("apply: only on Windows")
}
