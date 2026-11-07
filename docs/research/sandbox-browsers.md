# Browsers in an agent sandbox session

Measured 2026-11-07, for the question "can an agent run a browser (Playwright, a browser
MCP server) in a sandbox session?". Nothing here is shipped yet.

## Today: no browser runs

The sandbox image has none of the libraries a browser needs (libnss3, libatk, libgbm,
libcups, libxkbcommon, fonts...). `npx playwright install --with-deps` installs them with
apt as root, and a session has no root and a read-only `/`. So a browser fails to start in
a session, in every network mode.

## Then: the network refuses their TLS

Every ClientHello from these browsers carries an Encrypted ClientHello extension (0xfe0d),
a random one (GREASE) when the site offers no ECH config. The session's network refuses any
ECH extension in `dev` and `locked`: a real one hides the server name the allowlist checks,
and a GREASE one cannot be told from it by design. Allowing it would let a client reach any
site behind a CDN an allowed name shares (inner name hidden, outer name allowed).

The ClientHello was read by a listener that prints extension types, the browser pointed at
it with `--host-resolver-rules`:

| Browser | GREASE ECH | Turned off by |
|---|---|---|
| Chrome 157 (Windows) | yes | `--disable-features=EncryptedClientHello` and `...Grease`: **no effect** |
| Debian chromium 156 | yes | policy `EncryptedClientHelloEnabled: false` in `/etc/chromium/policies/managed` |
| Playwright 1.65, channel `chromium` (Chrome for Testing 158) | yes | the same policy in `/etc/opt/chrome_for_testing/policies/managed` only |
| Playwright 1.65, channel `chrome` (Chrome 157) | yes | the same policy in `/etc/opt/chrome/policies/managed` |
| Playwright 1.65, default headless (headless shell 158) | yes | **nothing**: it reads no policy in any of the three folders, and Playwright has no environment variable that picks another build |
| Playwright 1.65 Firefox 159 | yes | not `/etc/firefox/policies` nor `distribution/policies.json`; the preference `security.tls.ech.grease_probability = 0` (`firefoxUserPrefs`) does |

Non-browser clients (curl, Node's fetch, Python, Go, git) send no ECH; they work in `dev`.

## What supporting browsers would take

1. The browser libraries in the image (a few tens of MiB more), so Playwright's browsers
   start without root.
2. The three policy files above, read-only in the image.
3. A note, and a hint from `sandbox blocked` on an `ech` line: Playwright's default headless
   needs `channel: "chromium"`; Firefox needs the preference.
