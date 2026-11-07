# Browsers in an agent sandbox session

Measured 2026-11-07, for "can an agent run a browser (Playwright, a browser MCP server) in a
sandbox session?". The result is in the image and [sandbox.md](../sandbox.md#browsers).

## What a browser needs from the image

A session has no root and a read-only `/`, so `npx playwright install --with-deps` cannot
add libraries. Before this work the image had none, and no browser started.

Playwright 1.65's `install-deps --dry-run chromium` on Debian 13 lists 23 libraries, xvfb and
nine font packages. The image takes the libraries, `fonts-liberation`,
`fonts-noto-color-emoji` and `fonts-wqy-zenhei` (CJK, Hangul included); not xvfb (headless
only) and not the rest of the fonts.

`libgbm1` depends on Mesa's GPU drivers: `libLLVM.so.19.1` 124 MiB, `libgallium` 41 MiB,
`libz3` 27 MiB. The VM has no GPU. With those files deleted, both Chromium builds started,
drew Hangul and emoji, and created a WebGL context (Chromium's own software renderer). The
image leaves them out by `path-exclude`: system.vhd grew from 369 to 409 MiB, against 500 MiB
with them.

Firefox needs GTK 3 and libavcodec61 as well; it is not in the image.

## Downloading the browser

`npx playwright install chromium` in a `dev` session: `cdn.playwright.dev` redirects Chrome
for Testing to `storage.googleapis.com`. FFmpeg comes from `cdn.playwright.dev` after a try
at `playwright.download.prss.microsoft.com`. Neither host is in `dev`. Google Cloud Storage
takes uploads into any bucket its owner opens to everyone, so it stays out of the preset.

## The network refuses their TLS

Every ClientHello from these browsers carries an Encrypted ClientHello extension (0xfe0d), a
random one (GREASE) when the site offers no ECH config. `dev` and `locked` refuse any ECH
extension: a real one hides the server name the allowlist checks, and GREASE cannot be told
from it by design. Allowing it would let a client reach any site behind a CDN an allowed name
shares (inner name hidden, outer name allowed).

Read by a listener that prints extension types, the browser pointed at it with
`--host-resolver-rules`:

| Browser | GREASE ECH | Turned off by |
|---|---|---|
| Chrome 157 (Windows) | yes | `--disable-features=EncryptedClientHello` and `...Grease`: **no effect** |
| Debian chromium 156 | yes | policy `EncryptedClientHelloEnabled: false` in `/etc/chromium/policies/managed` |
| Playwright, channel `chromium` (Chrome for Testing 158) | yes | the same policy in `/etc/opt/chrome_for_testing/policies/managed` only |
| Playwright, channel `chrome` (Chrome 157) | yes | the same policy in `/etc/opt/chrome/policies/managed` |
| Playwright, default headless (headless shell 158) | yes | **nothing**: it reads no policy, and Playwright has no environment variable that picks another build |
| Playwright Firefox 159 | yes | not `/etc/firefox/policies` nor `distribution/policies.json`; the preference `security.tls.ech.grease_probability = 0` does |

In a session built with the policy files, `dev` mode, github.com: channel `chromium` loaded
the page; the headless shell got `net::ERR_CONNECTION_RESET`, and `sandbox blocked` showed
`20.200.245.247:443 ech`.

Non-browser clients (curl, Node's fetch, Python, Go, git) send no ECH.
