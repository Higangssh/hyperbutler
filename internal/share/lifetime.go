package share

// Share lifetime (ADR 0007, integration step 5). A bind source becomes a share on the
// first container create that names it; the share is removed once no container (in any
// state) has a bind mount from it. The engine learns which shares containers use from
// the guest dockerd's container list, which it re-reads when a container is destroyed
// and periodically (Registry.Prune).

import (
	"encoding/json"
	"errors"
	"strings"
)

// MaxContainerList bounds the GET /containers/json?all=1 reply SharesInUse accepts.
const MaxContainerList = 32 << 20

// SharesInUse returns the names of the shares that containers' bind mounts come from,
// given the guest dockerd's GET /containers/json?all=1 reply. The reply comes from the
// guest and is untrusted: a guest that lies can only keep shares alive (the user's own
// bind sources, until the engine stops) or let them be removed early (its own
// containers then lose them).
func SharesInUse(list []byte, mountPoint string) (map[string]bool, error) {
	if len(list) > MaxContainerList {
		return nil, errors.New("share: container list too large")
	}
	var cs []struct {
		Mounts []struct {
			Type   string `json:"Type"`
			Source string `json:"Source"`
		} `json:"Mounts"`
	}
	if err := json.Unmarshal(list, &cs); err != nil {
		return nil, err
	}
	used := map[string]bool{}
	prefix := strings.TrimRight(mountPoint, "/") + "/"
	for _, c := range cs {
		for _, m := range c.Mounts {
			if m.Type != "bind" || !strings.HasPrefix(m.Source, prefix) {
				continue
			}
			name, _, _ := strings.Cut(m.Source[len(prefix):], "/")
			if checkName(name) == nil {
				used[name] = true
			}
		}
	}
	return used, nil
}
