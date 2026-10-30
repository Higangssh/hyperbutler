package share

// Share lifetime (ADR 0007, integration step 5). A bind source becomes a share on the
// first container create that names it; the share is removed once no container (in any
// state) has a bind mount from it. The engine learns which shares containers use from
// the guest dockerd's container list, which it re-reads when a container is destroyed
// and periodically (Registry.Prune).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// shareName is stable for a host path, so the same source maps to the same share.
func shareName(key string) string {
	h := sha256.Sum256([]byte(strings.ToLower(key)))
	return "b-" + hex.EncodeToString(h[:8])
}

// isShareName reports whether s has the form shareName makes: "b-" and 16 lowercase
// hex digits.
func isShareName(s string) bool {
	if len(s) != 18 || !strings.HasPrefix(s, "b-") {
		return false
	}
	for _, c := range s[2:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

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
			if isShareName(name) {
				used[name] = true
			}
		}
	}
	return used, nil
}
