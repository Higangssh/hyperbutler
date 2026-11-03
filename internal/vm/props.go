package vm

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Higangssh/hyperbutler/internal/guid"
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

// Summary is one entry of an HcsEnumerateComputeSystems result.
type Summary struct {
	ID    string `json:"Id"`
	Owner string `json:"Owner"`
	State string `json:"State"`
}

// maxSummaries bounds the entries parseSummaries returns.
const maxSummaries = 4096

// parseSummaries decodes an HcsEnumerateComputeSystems result: a JSON array of
// properties documents. An empty or "null" document is an empty list.
func parseSummaries(doc string) ([]Summary, error) {
	if len(doc) > maxPropertiesLen {
		return nil, errors.New("vm: enumeration document is too large")
	}
	if doc == "" || doc == "null" {
		return nil, nil
	}
	var list []Summary
	if err := json.Unmarshal([]byte(doc), &list); err != nil {
		return nil, fmt.Errorf("vm: decode enumeration: %w", err)
	}
	if len(list) > maxSummaries {
		return nil, fmt.Errorf("vm: enumeration lists %d systems, more than %d", len(list), maxSummaries)
	}
	return list, nil
}

// parseState extracts State from an HcsGetComputeSystemProperties result.
func parseState(doc string) (string, error) {
	if len(doc) > maxPropertiesLen {
		return "", errors.New("vm: properties document is too large")
	}
	var p struct {
		State   string
		Stopped bool // a system that has exited reports this instead of State
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return "", fmt.Errorf("vm: decode properties: %w", err)
	}
	if p.State == "" && p.Stopped {
		return "Stopped", nil
	}
	if p.State == "" {
		if len(doc) > 300 {
			doc = doc[:300] + "…"
		}
		return "", fmt.Errorf("vm: properties have no State: %s", doc)
	}
	return p.State, nil
}
