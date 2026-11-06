package egress

import (
	"container/list"
	"net/netip"
	"time"
)

// pinTable maps an address to the names an answer gave it, each with its own expiry. It
// holds at most max pairs: a wildcard's names can answer with whatever addresses their
// owner likes, so the oldest pair makes room for a new one. A dropped pin comes back with
// the name's next answer. The caller locks.
type pinTable struct {
	max    int
	byAddr map[netip.Addr]map[string]*list.Element
	// order holds *pin, least recently renewed first.
	order list.List
}

type pin struct {
	addr    netip.Addr
	name    string
	expires time.Time
}

func newPinTable(limit int) pinTable {
	return pinTable{max: limit, byAddr: map[netip.Addr]map[string]*list.Element{}}
}

// put adds a pin or renews one.
func (t *pinTable) put(addr netip.Addr, name string, expires, now time.Time) {
	t.sweep(now)
	if el := t.byAddr[addr][name]; el != nil {
		el.Value.(*pin).expires = expires
		t.order.MoveToBack(el)
		return
	}
	for t.order.Len() >= t.max {
		t.remove(t.order.Front())
	}
	names := t.byAddr[addr]
	if names == nil {
		names = map[string]*list.Element{}
		t.byAddr[addr] = names
	}
	names[name] = t.order.PushBack(&pin{addr: addr, name: name, expires: expires})
}

// live returns the names addr is pinned for now.
func (t *pinTable) live(addr netip.Addr, now time.Time) []string {
	t.sweep(now)
	var out []string
	for name, el := range t.byAddr[addr] {
		if now.Before(el.Value.(*pin).expires) {
			out = append(out, name)
		}
	}
	return out
}

// has reports whether addr is pinned for name now.
func (t *pinTable) has(addr netip.Addr, name string, now time.Time) bool {
	el := t.byAddr[addr][name]
	return el != nil && now.Before(el.Value.(*pin).expires)
}

// sweep drops expired pins from the old end. Expiry is checked again on every use, so a
// clock that stepped back only delays this.
func (t *pinTable) sweep(now time.Time) {
	for el := t.order.Front(); el != nil && !now.Before(el.Value.(*pin).expires); el = t.order.Front() {
		t.remove(el)
	}
}

func (t *pinTable) remove(el *list.Element) {
	p := t.order.Remove(el).(*pin)
	names := t.byAddr[p.addr]
	delete(names, p.name)
	if len(names) == 0 {
		delete(t.byAddr, p.addr)
	}
}
