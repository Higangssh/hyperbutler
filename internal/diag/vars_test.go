package diag

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublish(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	c := srv.Client()
	defer c.CloseIdleConnections()
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	if code, _ := get(VarsPath + "test"); code != 404 {
		t.Fatalf("unpublished name: %d, want 404", code)
	}
	un := Publish("test", func() any { return map[string]int{"n": 1} })
	if code, body := get(VarsPath + "test"); code != 200 || body != `{"n":1}` {
		t.Fatalf("published: %d %s", code, body)
	}
	// A later Publish of the same name replaces it; the first unpublish then leaves
	// the second in place.
	un2 := Publish("test", func() any { return 2 })
	un()
	if code, body := get(VarsPath + "test"); code != 200 || body != "2" {
		t.Fatalf("replaced: %d %s", code, body)
	}
	un2()
	if code, _ := get(VarsPath + "test"); code != 404 {
		t.Fatalf("after unpublish: %d, want 404", code)
	}
}
