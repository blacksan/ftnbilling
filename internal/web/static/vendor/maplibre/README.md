# MapLibre GL JS (vendored)

Used by the Network page (`/admin/network`, `static/netmap.js`). Served from this server; nothing is loaded from a CDN.

- Package: `maplibre-gl` **6.19.0** (npm `latest` when vendored, 2026-11-24), BSD-3-Clause (see `LICENSE`).
- Tarball integrity (`npm view maplibre-gl@6.19.0 dist.integrity`, checked against the downloaded `.tgz` with sha512):
  `sha512-3bOrnfddggUYudLGJFsYFvqulUyvSxy4iQzd/uURI7ZhgYEZifdSrECujRSghaCpc+v/ZsFI0Y6ZsGSilEtE+w==`
- Files, copied unmodified from the tarball (`package/dist/` and `package/LICENSE.txt`):

| File | From | sha256 |
|---|---|---|
| `maplibre-gl.mjs` | `dist/maplibre-gl.mjs` | `01590c9875ea5bb0e8cdc534479f607f7d1695ca5debbccaf730edf5fa5f39e5` |
| `maplibre-gl-worker.mjs` | `dist/maplibre-gl-worker.mjs` | `c8564c4a4147326a587c9cc4c2a871c4b02a6c37c3f1ea51da55be3220d4b112` |
| `maplibre-gl.css` | `dist/maplibre-gl.css` | `a90798a1953017790e590f7e1bd35ff103a8a4263972cb47290b78adf86f8c20` |
| `LICENSE` | `LICENSE.txt` | `ee5fc05a0677eaf69601d2c7db0d9ecd6cc27c3abc1d0733bc9ed34707cf8ef2` |

## Why `.mjs` and no separate "CSP build"

MapLibre GL JS 6 ships ES modules only: the old UMD `maplibre-gl.js` and `maplibre-gl-csp.js` / `maplibre-gl-csp-worker.js`
builds are gone (v5.24.1 is the last line that has them). The v6 bundle uses no `eval` or `new Function`, so it runs under
`script-src 'self'`, and its worker is a module worker loaded from this origin (`worker-src 'self'`, no `blob:`).
`netmap.js` loads the library with a dynamic `import()` of the versioned URL and points `setWorkerUrl` at the worker here.
The `.map` source maps and `-dev` builds are not vendored (the `sourceMappingURL` comment at the end of the two scripts
then 404s in the browser's devtools, which is harmless).

## Updating

In an empty scratch directory: `npm pack maplibre-gl@<version>` (never run its scripts), compare the sha512 of the `.tgz`
with `npm view maplibre-gl@<version> dist.integrity`, copy the four files above over these, and update this file.
Run `go test ./internal/web/` (the Network tests check the files are present and that `netmap.js` builds no HTML).
