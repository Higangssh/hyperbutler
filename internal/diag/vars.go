package diag

import (
	"encoding/json"
	"net/http"
	"sync"
)

// VarsPath is the prefix under which Handler serves published documents:
// VarsPath + name.
const VarsPath = "/debug/vars/"

var vars struct {
	mu sync.Mutex
	m  map[string]*varEntry
}

type varEntry struct{ f func() any }

// Publish serves f's result as JSON at VarsPath+name on this process's debug endpoint,
// so a package can expose its own counters without the endpoint knowing about them.
// It costs nothing while the endpoint is off: f is only called for a request. The
// returned function removes it again, and only if name still serves this f.
func Publish(name string, f func() any) (unpublish func()) {
	e := &varEntry{f: f}
	vars.mu.Lock()
	if vars.m == nil {
		vars.m = map[string]*varEntry{}
	}
	vars.m[name] = e
	vars.mu.Unlock()
	return func() {
		vars.mu.Lock()
		if vars.m[name] == e {
			delete(vars.m, name)
		}
		vars.mu.Unlock()
	}
}

func serveVar(w http.ResponseWriter, r *http.Request) {
	vars.mu.Lock()
	e := vars.m[r.PathValue("name")]
	vars.mu.Unlock()
	if e == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(e.f())
}
