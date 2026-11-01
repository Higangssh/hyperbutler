package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/Higangssh/hyperbutler/internal/config"
)

// Confirmation tokens. The operations that stop something or change what dockerd runs
// (TokenActions) are refused unless the request carries a token from a "confirm" request
// for that action (and, for a container, that container). The host issues a token only
// after it has itself computed what the action stops: for the engine and dockerd, the
// REQ-UX-017 list of running containers, which the page shows in its dialog. A token is
// good once and for TokenTTL.
//
// What this guarantees: no destructive operation runs without that list having been
// computed by the host and handed to the page, and a page cannot replay or reuse a
// confirmation. What it cannot guarantee: that a person clicked. A page made to run
// script could ask for a token and send it straight back; only a native dialog drawn
// by the host could prove a click, at the cost of every confirmation appearing twice.
// The page has no HTML sink and the CSP's Trusted Types refuse one, which is what keeps
// script out of it in the first place (docs/gui.md).

// TokenActions are the actions that need a confirmation token.
var TokenActions = map[string]bool{
	OpEngineStop:        true,
	OpEngineRestart:     true,
	OpDockerdRestart:    true,
	OpDaemonSave:        true,
	"container.stop":    true,
	"container.restart": true,
	OpImportRun:         true,
	OpConfigSet:         true, // only the settings Editable marks Confirmed
}

// TokenTTL is how long a token stays good. Every token gets the same ten minutes: the
// list a stop or restart shows (REQ-UX-017) can be long, and a person who reads it, or
// steps away with the dialog open, should not come back to HB-GUI-005. A shorter life
// would protect nothing: a token is spent on its first use (or refusal), is bound to
// one action (and container, or setting and value), and a page that could misuse one
// could as well ask for a fresh one. Expiry only bounds what is outstanding, with
// maxTokens.
const TokenTTL = 10 * time.Minute

// maxTokens bounds the outstanding tokens; the oldest go first.
const maxTokens = 32

// CodeConfirm: a destructive request without a valid confirmation token.
const CodeConfirm = "HB-GUI-005"

// ConfirmArgs ask for a token for an action (and a container, for container.*; a setting
// and its new value, for config.set).
type ConfirmArgs struct {
	Action string `json:"action"`
	ID     string `json:"id,omitempty"`
	Key    string `json:"key,omitempty"`
	Value  string `json:"value,omitempty"`
}

// TokenArgs carry a confirmation token.
type TokenArgs struct {
	Token string `json:"token"`
}

var reToken = regexp.MustCompile(`^[0-9a-f]{32}$`)

func checkToken(tok string) error {
	if !reToken.MatchString(tok) {
		return errors.New("token: missing or not a confirmation token")
	}
	return nil
}

func checkConfirmArgs(a *ConfirmArgs) error {
	if !TokenActions[a.Action] {
		return fmt.Errorf("action %.40q needs no confirmation", a.Action)
	}
	isContainer := a.Action == "container.stop" || a.Action == "container.restart"
	isSetting := a.Action == OpConfigSet
	switch {
	case isContainer && !reContainerID.MatchString(a.ID):
		return errors.New("id: want a container id (64 hex digits)")
	case !isContainer && a.ID != "":
		return errors.New("id: only container actions name one")
	case isSetting && !reKey.MatchString(a.Key):
		return errors.New("key: not a setting name")
	case isSetting && (len(a.Value) > maxValue || !printableValue(a.Value)):
		return fmt.Errorf("value: at most %d bytes of printable text", maxValue)
	case !isSetting && (a.Key != "" || a.Value != ""):
		return errors.New("key, value: only config.set names them")
	}
	return nil
}

// settingID is what a config.set token is bound to: the setting and its new value (a
// setting's name has no "=").
func settingID(key, value string) string { return key + "=" + value }

type token struct {
	action, id string
	expires    time.Time
}

type tokens struct {
	mu    sync.Mutex
	m     map[string]token
	order []string
	now   func() time.Time
}

func (t *tokens) issue(action, id string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "" // never a usable token
	}
	s := hex.EncodeToString(b[:])
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]token{}
	}
	t.m[s] = token{action: action, id: id, expires: t.now().Add(TokenTTL)}
	t.order = append(t.order, s)
	for len(t.order) > maxTokens {
		delete(t.m, t.order[0])
		t.order = t.order[1:]
	}
	return s
}

// use spends tok for action and id; it is gone afterwards whether or not it matched.
// Callers hold the exclusive lock already (Dispatcher.exclusive), so a request that is
// refused as busy keeps its token.
func (t *tokens) use(tok, action, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	k, ok := t.m[tok]
	delete(t.m, tok)
	switch {
	case !ok:
		return Codedf(CodeConfirm, "%s needs a confirmation, and this one is unknown or used", action)
	case t.now().After(k.expires):
		return Codedf(CodeConfirm, "%s: the confirmation expired", action)
	case k.action != action || k.id != id:
		return Codedf(CodeConfirm, "the confirmation was for another action")
	}
	return nil
}

// ConfirmView is a token with what the action will stop (engine and dockerd actions).
type ConfirmView struct {
	Token   string       `json:"token"`
	Preview *PreviewView `json:"preview,omitempty"`
}

func (d *Dispatcher) confirm(ctx context.Context, a *ConfirmArgs) (any, error) {
	v := ConfirmView{}
	switch a.Action {
	case OpEngineStop, OpEngineRestart, OpDockerdRestart:
		// REQ-UX-017: the list is computed here, before any token exists.
		c, cancel := context.WithTimeout(ctx, 8*time.Second)
		p, err := d.preview(c)
		cancel()
		if err != nil {
			return nil, err
		}
		pv := p.(PreviewView)
		v.Preview = &pv
	case OpConfigSet:
		k, err := config.Lookup(a.Key)
		if err != nil {
			return nil, Coded(CodeConfigValue, err)
		}
		if Editable[k.Name] != Confirmed {
			return nil, Codedf(CodeNotEditable, "%s needs no confirmation in the window, or is changed in a terminal", k.Name)
		}
		v.Token = d.tok.issue(a.Action, settingID(k.Name, a.Value))
		if v.Token == "" {
			return nil, Codedf(CodeConfirm, "no confirmation could be made")
		}
		return v, nil
	}
	v.Token = d.tok.issue(a.Action, a.ID)
	if v.Token == "" {
		return nil, Codedf(CodeConfirm, "no confirmation could be made")
	}
	return v, nil
}
