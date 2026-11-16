package daemoncfg

import (
	"bytes"
	"encoding/json"
)

// The build cache policy hyperbutler gives dockerd's BuildKit when the user's
// daemon.json does not configure builder.gc (REQ-DISK-006, ADR 0011). dockerd parses
// the sizes with units.RAMInBytes, so "GB" is GiB: the cache is pruned above 8 GiB
// (8.6 GB, under the requirement's 10 GB with room for what a build adds before the next
// collection), and down towards 2 GiB while the data disk has less than 5 GiB free.
// Without it dockerd's policy scales with the disk: up to 80 % of it.
const (
	BuildCacheMax      = "8GB"
	BuildCacheReserved = "2GB"
	BuildCacheMinFree  = "5GB"
)

// gcDefault is the "gc" value WithDefaults adds.
var gcDefault = []byte(`{"enabled":true,"defaultReservedSpace":"` + BuildCacheReserved +
	`","defaultMaxUsedSpace":"` + BuildCacheMax + `","defaultMinFreeSpace":"` + BuildCacheMinFree + `"}`)

// DefaultsRoom is the most WithDefaults adds to a file. Check keeps user files that much
// under MaxSize, so the delivered file always fits the guest's limit.
const DefaultsRoom = 256

// WithDefaults returns the daemon.json to deliver for the user's file b. A file that
// configures builder.gc goes through unchanged; otherwise hyperbutler's build cache
// policy is added as "builder":{"gc":...} (no "builder" key) or as "gc" inside the
// user's "builder" object (one that sets entitlements or history only). A "builder"
// that is not an object is left to dockerd. A nil b (no user file) gives a file with
// only the default. b must have passed Check; the user's bytes are kept and the key is
// inserted before the closing brace of the object it goes into.
func WithDefaults(b []byte) []byte {
	if b == nil {
		return append(append([]byte(`{"builder":{"gc":`), gcDefault...), "}}\n"...)
	}
	top, ok := objectKeys(b, 0)
	if !ok {
		return b
	}
	for _, k := range top.keys {
		if fold(k.name) != fold("builder") {
			continue
		}
		inner, ok := objectKeys(b[:k.end], k.start)
		if !ok {
			return b // null, or not an object: dockerd's to judge
		}
		for _, ik := range inner.keys {
			if fold(ik.name) == fold("gc") {
				return b
			}
		}
		return insert(b, inner.close, len(inner.keys) > 0, `"gc":`, gcDefault)
	}
	return insert(b, top.close, len(top.keys) > 0,
		`"builder":{"gc":`, append(append([]byte{}, gcDefault...), '}'))
}

func insert(b []byte, at int, comma bool, key string, value []byte) []byte {
	out := make([]byte, 0, len(b)+len(key)+len(value)+1)
	out = append(out, b[:at]...)
	if comma {
		out = append(out, ',')
	}
	out = append(out, key...)
	out = append(out, value...)
	return append(out, b[at:]...)
}

// objKey is one key of a JSON object and the byte range of its value in the document.
type objKey struct {
	name       string
	start, end int
}

type object struct {
	keys  []objKey
	close int // offset of the closing brace
}

// objectKeys reads the JSON object that starts (after white space) at off in b, which
// ends where the object does (or later, with only white space after it). ok is false
// if it is not an object.
func objectKeys(b []byte, off int) (o object, ok bool) {
	d := json.NewDecoder(bytes.NewReader(b[off:]))
	d.UseNumber()
	tok, err := d.Token()
	if err != nil {
		return o, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return o, false
	}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return o, false
		}
		name, isString := tok.(string)
		if !isString {
			return o, false
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return o, false
		}
		end := off + int(d.InputOffset())
		o.keys = append(o.keys, objKey{name: name, start: end - len(v), end: end})
	}
	if _, err := d.Token(); err != nil {
		return o, false
	}
	o.close = off + int(d.InputOffset()) - 1
	if o.close < 0 || o.close >= len(b) || b[o.close] != '}' {
		return o, false
	}
	return o, true
}
