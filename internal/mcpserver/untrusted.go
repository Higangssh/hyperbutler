package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Higangssh/hyperbutler/internal/redact"
)

// Text that dockerd, the guest, a registry or Windows wrote never reaches the client
// outside a field named untrustedText (docs/mcp.md, "What it returns"). In results it is
// put there by each tool; in errors, by untrustedError: the error's own text is this
// server's, and what the other side said travels next to it.

// maxUntrusted is the most bytes of one untrusted text in an error or a status field.
const maxUntrusted = 4 << 10

// untrustedError is an error whose message is this server's and whose text was written
// by dockerd, the guest, a registry or Windows. Error() is only the message; errorResult
// puts the text in untrustedText.
type untrustedError struct {
	msg  string
	text string
	err  error // wrapped, for errors.Is; may be nil
}

func (e *untrustedError) Error() string { return e.msg }
func (e *untrustedError) Unwrap() error { return e.err }

// untrustedText is the text of the innermost untrusted error in err's chain, or "".
func untrustedText(err error) string {
	var ue *untrustedError
	var ae *apiError
	switch {
	case errors.As(err, &ue):
		return ue.text
	case errors.As(err, &ae):
		return ae.Message
	}
	return ""
}

// fromOther wraps err, which a command of the host or the Windows API returned, so its
// text is labeled: msg says what failed.
func fromOther(msg string, err error) error {
	if err == nil {
		return nil
	}
	return &untrustedError{msg: msg, text: err.Error(), err: err}
}

// boundText is text for the client: redacted, cleaned and at most n bytes.
func boundText(s string, multiline bool, n int) string {
	s = cleanText(redact.String(s), multiline)
	if len(s) > n {
		s = clip(s, n) + " [truncated]"
	}
	return s
}

// errorJSON is a failed call whose error carries untrusted text.
type errorJSON struct {
	Error         string `json:"error"`
	UntrustedNote string `json:"untrustedNote"`
	UntrustedText string `json:"untrustedText"`
}

// errorText is a failed call's text as the client sees it: the error's own message, and,
// when it carries text another side wrote, a JSON object with that text in
// untrustedText.
func errorText(err error) string {
	msg := boundText(err.Error(), true, maxStatusText)
	u := untrustedText(err)
	if u == "" {
		return msg
	}
	b, jerr := redactedJSON(errorJSON{Error: msg, UntrustedNote: untrustedNote, UntrustedText: boundText(u, true, maxUntrusted)})
	if jerr != nil {
		return msg
	}
	return string(b)
}

// --- status ---------------------------------------------------------------------------

// UntrustedField is a field of the status document whose text dockerd, the guest or
// Windows wrote, moved out of its place: Field is where it was ("state.error").
type UntrustedField struct {
	Field         string `json:"field"`
	UntrustedText string `json:"untrustedText"`
}

// statusResult is the status --json document with its free-text fields (state.error,
// state.daemonConfig.error, state.daemonConfig.env.error, state.hostGateway.error and
// each state.unpublishedPorts[].reason) moved into untrustedFields, next to an
// untrustedNote. Everything else is passed through.
func statusResult(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("status: decode the document: %w", err)
	}
	var moved []UntrustedField
	take := func(obj any, key, path string) {
		o, ok := obj.(map[string]any)
		if !ok {
			return
		}
		v, ok := o[key]
		if !ok {
			return
		}
		delete(o, key)
		s, _ := v.(string)
		if s == "" {
			return
		}
		moved = append(moved, UntrustedField{Field: path, UntrustedText: boundText(s, true, maxUntrusted)})
	}
	if st, ok := m["state"].(map[string]any); ok {
		take(st, "error", "state.error")
		if dc, ok := st["daemonConfig"].(map[string]any); ok {
			take(dc, "error", "state.daemonConfig.error")
			take(dc["env"], "error", "state.daemonConfig.env.error")
		}
		take(st["hostGateway"], "error", "state.hostGateway.error")
		if ups, ok := st["unpublishedPorts"].([]any); ok {
			for i, u := range ups {
				take(u, "reason", fmt.Sprintf("state.unpublishedPorts[%d].reason", i))
			}
		}
	}
	if len(moved) > 0 {
		m["untrustedNote"] = untrustedNote
		m["untrustedFields"] = moved
	}
	return m, nil
}
