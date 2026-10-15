# orch web dashboard

The Svelte single-page app served by `orch web`. The build output in `dist/`
is committed and embedded into the `orch` binary, so `go build` needs no
JavaScript toolchain.

```console
make web          # install, type-check, and rebuild dist/
make web-check    # CI: fail when dist/ is stale
```

For live reloading, run `orch web --port 7070 --no-open`, open the printed URL
once so the session cookie is set, then run `bun run dev` here and open
http://127.0.0.1:5173 (not `localhost`, so the cookie applies). Vite proxies
`/api` to port 7070.
