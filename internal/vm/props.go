package vm

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// maxPropertiesLen bounds the properties document parseRuntimeID accepts. HCS documents
// are a few kilobytes; this only stops a pathological one from being decoded.
const maxPropertiesLen = 1 << 20

// parseRuntimeID extracts RuntimeId from an HcsGetComputeSystemProperties result.
func parseRuntimeID(doc string) (guid.GUID, error) {
	if len(doc) > maxPropertiesLen {
		return guid.GUID{}, errors.New("vm: properties document is too large")
	}
	var p struct{ RuntimeId string }
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return guid.GUID{}, fmt.Errorf("vm: decode properties: %w", err)
	}
	if p.RuntimeId == "" {
		return guid.GUID{}, errors.New("vm: properties have no RuntimeId")
	}
	id, err := guid.FromString(p.RuntimeId)
	if err != nil {
		return guid.GUID{}, fmt.Errorf("vm: RuntimeId %q: %w", p.RuntimeId, err)
	}
	if id == (guid.GUID{}) {
		return guid.GUID{}, errors.New("vm: RuntimeId is the nil GUID")
	}
	return id, nil
}
