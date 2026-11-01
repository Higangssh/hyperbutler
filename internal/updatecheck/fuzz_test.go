package updatecheck

import (
	"strings"
	"testing"
)

// FuzzParseRelease: the release document comes from the network. Whatever it holds,
// the result is a version and a page of this repository, and the version order is
// consistent.
func FuzzParseRelease(f *testing.F) {
	f.Add([]byte(`{"tag_name":"v0.3.0","html_url":"https://github.com/Higangssh/hyperbutler/releases/tag/v0.3.0"}`), "v0.2.0")
	f.Add([]byte(`{"tag_name":"v0.3.0-rc.1","html_url":"https://evil.example"}`), "v0.3.0-rc.01")
	f.Add([]byte(`{"tag_name":"1.2.3+x"}`), "(devel)")
	f.Add([]byte(`null`), "")
	f.Fuzz(func(t *testing.T, body []byte, version string) {
		latest, page, err := ParseRelease(body)
		if err != nil {
			if latest != "" || page != "" {
				t.Fatalf("error with a result: %q %q", latest, page)
			}
		} else {
			if !strings.HasPrefix(page, releasePagePrefix) || page != releasePagePrefix+latest {
				t.Fatalf("page %q for %q", page, latest)
			}
			if strings.ContainsFunc(page, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				t.Fatalf("control character in %q", page)
			}
			if Newer(latest, latest) {
				t.Fatalf("%q newer than itself", latest)
			}
		}
		if Newer(latest, version) && Newer(version, latest) {
			t.Fatalf("%q and %q are each newer", latest, version)
		}
		_ = Notice(Record{Latest: latest, URL: page}, version)
	})
}
