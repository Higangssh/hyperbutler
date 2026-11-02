# hyperbutler website

The landing page for hyperbutler: plain HTML and CSS, no build step, no JavaScript, no web
fonts, no analytics. The page loads nothing from any other host.

| File | What it is |
|---|---|
| `index.html` | The page |
| `style.css` | All styles (no inline styles, so the CSP needs no `'unsafe-inline'`) |
| `img/` | Copies of files from `assets/brand/` (banner, mascot, icons, Open Graph image) |
| `_headers` | Security and cache headers, applied by Cloudflare Pages (not served) |
| `robots.txt` | Allows all crawlers |

Facts on the page come from the repository's `README.md` ("Current status") and
`docs/install.md`. When those change, update the page by hand. If an asset in
`assets/brand/` changes, copy it into `img/` again.

## Preview locally

Open `index.html` in a browser, or serve the folder, for example:

```
python -m http.server 8000 --directory site
```

## Deploy on Cloudflare Pages

1. Cloudflare dashboard: **Workers & Pages** > **Create** > **Pages** > **Connect to Git**.
2. Install or authorize the **Cloudflare GitHub app** for `Higangssh/hyperbutler` (it works
   whether the repository is private or public), and pick the repository.
3. Build settings:
   - Production branch: `main`
   - Framework preset: **None**
   - Build command: leave empty
   - Build output directory: `site`
4. **Save and Deploy.** The site is then at `https://<project>.pages.dev`, and every push to
   `main` redeploys it. Pushes to other branches get preview URLs; turn that off under
   **Settings** > **Builds & deployments** if you do not want them.

Note that every file in `site/` except `_headers` is served, this README included.

## Custom domain

1. In the Pages project: **Custom domains** > **Set up a custom domain**, and enter the domain
   bought on Cloudflare (or a subdomain of it). Because the zone is on Cloudflare, the DNS
   record and the certificate are created for you.
2. Optionally redirect `<project>.pages.dev` to the custom domain (a Bulk Redirect rule), so
   only one address is indexed.
3. Make the two URLs at the top of `index.html` absolute, in the comment block marked
   `SITE URL`: the `canonical` link and `og:image`. Some link-preview crawlers ignore a
   relative `og:image`. No other file names the domain.

## Security headers

`_headers` sets a strict Content-Security-Policy (`default-src 'none'`, images and the
stylesheet from the site itself only, no scripts), HSTS without `preload`, `nosniff`,
`frame-ancestors 'none'` and a restrictive Permissions-Policy. If you add a script, put it
in its own file and add `script-src 'self'`; do not allow inline scripts.
