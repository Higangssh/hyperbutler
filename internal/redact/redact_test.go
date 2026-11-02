package redact

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const s3 = "hunter2Secret9"

func TestString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"auths":{"registry.example":{"auth":"dXNlcjpwYXNz"}}}`, `{"auths":{"registry.example":{"auth":"[REDACTED]"}}}`},
		{`"identitytoken": "` + s3 + `"`, `"identitytoken": "[REDACTED]"`},
		{`{"password": 12345}`, `{"password": [REDACTED]}`},
		{`proxy http://bob:` + s3 + `@proxy.corp:3128/x`, `proxy http://[REDACTED]@proxy.corp:3128/x`},
		{`https://tok` + s3 + `@github.com/a/b`, `https://[REDACTED]@github.com/a/b`},
		{`http://u:p@ss` + s3 + `@h/`, `http://[REDACTED]@h/`},
		{`"httpProxy": "bob:` + s3 + `@proxy:3128"`, `"httpProxy": "[REDACTED]@proxy:3128"`},
		{"Authorization: Bearer " + s3 + "\nnext", "Authorization: [REDACTED]\nnext"},
		{`"Proxy-Authorization": "Basic Ym9iOnB3"`, `"Proxy-Authorization": "[REDACTED]"`},
		{"X-Registry-Auth: eyJ" + s3, "X-Registry-Auth: [REDACTED]"},
		{"got bearer " + s3 + " back", "got bearer [REDACTED] back"},
		{`level=INFO msg=login password=` + s3 + ` user=bob`, `level=INFO msg=login password=[REDACTED] user=bob`},
		{`token="a b ` + s3 + `" ok`, `token="[REDACTED]" ok`},
		{`GET /v2/?access_token=` + s3 + `&x=1`, `GET /v2/?access_token=[REDACTED]&x=1`},
		{`msg="{\"password\":\"` + s3 + `\"}"`, `msg="{\"password\":\"[REDACTED]\"}"`},
		{`docker login --password ` + s3 + ` reg`, `docker login --password [REDACTED] reg`},
		{"secret: " + s3, "secret: [REDACTED]"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----\nafter", "[REDACTED]\nafter"},
		{"-----BEGIN PRIVATE KEY-----\nMIIabc truncated", "[REDACTED]"},
		{"jwt eyJhbGciOiJIUzI1.eyJzdWIiOiIx.c2lnbmF0dXJl end", "jwt [REDACTED] end"},
		{"use ghp_0123456789abcdefghijABCDEFGHIJ now", "use [REDACTED] now"},
		{"AKIAABCDEFGHIJKLMNOP", "[REDACTED]"},
		// M-1: quoting, spacing and flag spellings.
		{`password='` + s3 + `' x`, `password='[REDACTED]' x`},
		{`export DOCKER_PASSWORD='` + s3 + `'`, `export DOCKER_PASSWORD='[REDACTED]'`},
		{`password = ` + s3, `password = [REDACTED]`},
		{`PASSWORD : ` + s3, `PASSWORD : [REDACTED]`},
		{`token:` + s3 + ` next`, `token:[REDACTED] next`},
		{`password: '` + s3 + `'`, `password: '[REDACTED]'`},
		{`docker login -u bob -p ` + s3 + ` reg.example`, `docker login -u [REDACTED] -p [REDACTED] reg.example`},
		{`docker login -p=` + s3 + ` reg`, `docker login -p=[REDACTED] reg`},
		{`docker login --username bob --password -` + s3, `docker login --username bob --password [REDACTED]`},
		{`--password "a b ` + s3 + `"`, `--password "[REDACTED]"`},
		{"{\n  \"password\":\n    \"" + s3 + "\",\n  \"x\": 1\n}", "{\n  \"password\":\n    \"[REDACTED]\",\n  \"x\": 1\n}"},
		{"\"auth\"\n  : \"" + s3 + "\"", "\"auth\"\n  : \"[REDACTED]\""},
		{"\"token\":\n\n" + s3 + "\n", "\"token\":\n\n[REDACTED]\n"},
		{"Authorization: Bearer ab=" + s3, "Authorization: [REDACTED]"},
		{"got bearer ab=" + s3, "got bearer [REDACTED]"},
		// Not secrets.
		{`docker run -p 8080:80 -u 1000 nginx`, `docker run -p 8080:80 -u 1000 nginx`},
		{"msg=\"reset password\"\n\"x\"", "msg=\"reset password\"\n\"x\""},
		{`C:\Users\bob\AppData\Local\hyperbutler\logs\engine.log`, `C:\Users\bob\AppData\Local\hyperbutler\logs\engine.log`},
		{`{"credsStore":"desktop","currentContext":"hyperbutler"}`, `{"credsStore":"desktop","currentContext":"hyperbutler"}`},
		{`level=INFO msg="engine starting" pid=42 home=C:\h memoryMB=4096`, `level=INFO msg="engine starting" pid=42 home=C:\h memoryMB=4096`},
		{`mail bob@example.com at 10:30`, `mail bob@example.com at 10:30`},
		{`npipe:////./pipe/docker_engine_hyperbutler`, `npipe:////./pipe/docker_engine_hyperbutler`},
		{"authorization failed", "authorization failed"},
	} {
		if got := String(tc.in); got != tc.want {
			t.Errorf("String(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// BenchmarkLog redacts 4 MiB of engine-log-like lines (diagnose reads up to that much of
// each log).
func BenchmarkLog(b *testing.B) {
	line := `time=2026-11-01T10:00:00.000+09:00 level=INFO msg="docker request" method=GET path=/v1.47/containers/json status=200 ms=1 vm=hyperbutler-1a2b3c4d-0011` + "\n"
	secret := `time=2026-11-01T10:00:00.000+09:00 level=WARN msg="pull" url=https://u:p@reg.example/v2/ token=abc "auth":"x"` + "\n"
	block := strings.Repeat(line, 49) + secret
	in := strings.Repeat(block, (4<<20)/len(block))
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		_ = String(in)
	}
}

// TestLongLines: a long line of matches costs linear time, not quadratic. It compares a
// line with the same line at a quarter of the length, so a slow machine or the race
// detector (about 10x slower) does not fail it: linear is about 4x, quadratic 16x.
func TestLongLines(t *testing.T) {
	timeOf := func(s string) time.Duration {
		t0 := time.Now()
		_ = String(s)
		return time.Since(t0)
	}
	for _, unit := range []string{`"token":"x",`, `token=x `, `"a":`, `"`, `\"auth\":\"x\" `, `http://a:b@`, `--token x `, `a:b@ `,
		`password = 'x' `, `token : x `, `login -p x `, `-p `, `"password":` + "\n", `'`, `password='`} {
		n := (1 << 20) / len(unit)
		small, large := timeOf(strings.Repeat(unit, n/4)), timeOf(strings.Repeat(unit, n))
		if large > 8*small+50*time.Millisecond {
			t.Errorf("%q: x %d took %v, x %d took %v: more than linear", unit, n/4, small, n, large)
		}
	}
}

func TestIsSecretKey(t *testing.T) {
	for k, want := range map[string]bool{
		"auth": true, "Authorization": true, "Proxy-Authorization": true, "identitytoken": true,
		"registrytoken": true, "HTTP_PROXY_PASSWORD": true, "client_secret": true, "apiKey": true,
		"x-registry-auth": true, "proxyAuth": true, "pwd": true,
		"auths": false, "credsStore": false, "credHelpers": false, "author": false, "dataDir": false,
		"pipe": false, "insecure-registries": false, "httpProxy": false,
	} {
		if got := IsSecretKey(k); got != want {
			t.Errorf("IsSecretKey(%q) = %v", k, got)
		}
	}
}

func TestJSON(t *testing.T) {
	in := `{
  "auths": {"https://bob:` + s3 + `@reg.example": {"auth": "dXNlcjpwYXNz", "identitytoken": {"nested": "x"}}},
  "proxies": {"http-proxy": "http://bob:` + s3 + `@p:3128", "no-proxy": "localhost"},
  "registry-mirrors": ["https://m:` + s3 + `@mirror.example"],
  "tokenAuth": false, "password": null, "debug": true, "n": 12345678901234567890
}`
	out := string(JSON([]byte(in)))
	if strings.Contains(out, s3) || strings.Contains(out, "dXNlcjpwYXNz") || strings.Contains(out, "nested") {
		t.Fatalf("a secret survived:\n%s", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	for _, want := range []string{`"no-proxy": "localhost"`, `"tokenAuth": false`, `"password": null`, `"debug": true`, `12345678901234567890`, `"https://[REDACTED]@reg.example"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	// Not JSON: redacted as text.
	if got := string(JSON([]byte(`{"auth":"` + s3 + `"`))); strings.Contains(got, s3) {
		t.Fatalf("invalid JSON kept the secret: %s", got)
	}
	if got := string(JSON([]byte(`{} {"auth":"` + s3 + `"}`))); strings.Contains(got, s3) {
		t.Fatalf("trailing data kept the secret: %s", got)
	}
}
