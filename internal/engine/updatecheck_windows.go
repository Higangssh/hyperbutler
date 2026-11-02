//go:build windows

package engine

import "golang.org/x/sys/windows/registry"

func init() { UpdateCheckDefault = machineUpdateCheck }

// machineUpdateCheck reads the installer's choice: a DWORD UpdateCheck under
// HKLM\SOFTWARE\hyperbutler (dist/msi), 0 for off. Without it (the zip, or the box left
// ticked) the check is on.
func machineUpdateCheck() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\hyperbutler`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetIntegerValue("UpdateCheck")
	return err != nil || v != 0
}
