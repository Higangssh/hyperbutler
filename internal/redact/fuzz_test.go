package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// templates place a secret (%s) the way registries, proxies and HTTP clients write one.
var templates = []string{
	`"auth":"%s"`,
	`{"auths":{"registry.example":{"auth":"%s"}}}`,
	`"identitytoken": "%s"`,
	`"password": %s`,
	`https://user:%s@registry.example/v2/`,
	`http://%s@proxy.example:3128`,
	` user:%s@proxy.example:3128`,
	`"httpsProxy":"bob:%s@proxy:3128"`,
	`Authorization: Bearer %s`,
	`Proxy-Authorization: Basic %s`,
	`X-Registry-Auth: %s`,
	`password=%s`,
	`token="%s"`,
	`?access_token=%s&x=1`,
	`msg="{\"password\":\"%s\"}"`,
	`--password %s`,
	`secret: %s`,
	`bearer %s`,
}

// FuzzString asserts that a secret placed in any template survives nowhere in the
// output, whatever text surrounds it.
func FuzzString(f *testing.F) {
	f.Add("", "", "hunter2Secret9", uint8(0))
	f.Add(`{"a":"`, `"}`, "dXNlcjpwYXNzd29yZA", uint8(1))
	f.Add(`level=INFO msg="x" `, " user=bob\n", "S3cr3tS3cr3t", uint8(11))
	f.Add(`"x\`, `\"`, "abcdefgh12", uint8(14))
	f.Add("password=", "", "abcdefgh12", uint8(0))
	f.Add("http://", "@x", "abcdefgh12", uint8(6))
	f.Fuzz(func(t *testing.T, prefix, suffix, seed string, kind uint8) {
		var sb strings.Builder
		for _, r := range seed {
			if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				sb.WriteRune(r)
			}
		}
		secret := sb.String()
		if len(secret) < 8 {
			return
		}
		secret = secret[:min(len(secret), 64)]
		tmpl := templates[int(kind)%len(templates)]
		if strings.Contains(prefix, secret) || strings.Contains(suffix, secret) || strings.Contains(tmpl, secret) {
			return
		}
		in := prefix + " " + strings.Replace(tmpl, "%s", secret, 1) + " " + suffix
		if out := String(in); strings.Contains(out, secret) {
			t.Fatalf("secret %q survived:\n in %q\nout %q", secret, in, out)
		}
		// The same in a JSON document: as a value next to the surrounding text, and in a
		// proxy URL.
		ps, _ := json.Marshal(prefix)
		if strings.Contains(string(JSON(ps)), secret) {
			return // the escaped text holds it ("\u0000" then "0000...")
		}
		doc := `{"x":` + string(ps) + `,"auths":{"r":{"auth":"` + secret + `"}},"proxies":{"http-proxy":"http://u:` + secret + `@p"}}`
		if out := JSON([]byte(doc)); strings.Contains(string(out), secret) {
			t.Fatalf("secret %q survived in JSON:\n in %s\nout %s", secret, doc, out)
		}
	})
}

// FuzzJSON: JSON never panics, and valid JSON stays valid JSON.
func FuzzJSON(f *testing.F) {
	f.Add([]byte(`{"auths":{"r":{"auth":"eA=="}},"credsStore":"desktop"}`))
	f.Add([]byte(`[1,"https://a:b@c",{"token":[1,2]}]`))
	f.Add([]byte(`not json password=x`))
	f.Fuzz(func(t *testing.T, b []byte) {
		out := JSON(b)
		if json.Valid(b) && !json.Valid(out) {
			t.Fatalf("valid JSON became invalid:\n in %q\nout %q", b, out)
		}
	})
}
