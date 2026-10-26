# orch website

The public product and documentation site is a prerendered
[SvelteKit](https://svelte.dev/docs/kit) app. The landing page lives in
`src/routes/+page.svelte`; documentation pages are rendered from the Markdown in
`../docs`, with the sidebar order taken from `docs/SUMMARY.md`. The versioned
Pkl schemas in `../pkl/v*/` are published unchanged at `/pkl/`, for example
`/pkl/v1alpha1/Application.pkl`. Dependencies are
pinned by `package.json` and `bun.lock`.

From this directory:

```bash
bun install --frozen-lockfile
bun run check
bun run dev
```

`./build.sh` type-checks the site, prerenders every page into `dist`, and adds
the Sites worker. The build fails on links to missing pages or headings.
Generated output is ignored and must not be edited.

## Deploy

`orch.pkl` deploys the site with orch as a
[static site](../docs/manifest.md#static-sites) serving `orch.torsten.work`.
Every push to `main` that touches `website/`, `docs/` or `pkl/` runs
`.github/workflows/website-deploy.yml`, which prerenders with
`bun run build:site` and deploys `dist` with orch. `build.sh` is not used for
the deploy: the Sites worker it adds would be served as a public file.

The workflow reads the `ORCH_CLUSTER` and `ORCH_VERSION` repository variables
and needs orch to trust this repository once:

```sh
orch github trust TorstenDittmann/orch --app orch-website --branch main --environment production
```

To deploy by hand from this directory:

```sh
bun run build:site
orch deploy -f orch.pkl --wait
```

The site's public origin defaults to `https://orch.torsten.work`. Set
`SITE_URL` while building to change it: it controls canonical links, the
sitemap, robots.txt and generated documentation URLs.

The Dockerfile still builds a self-contained image that serves `dist` on port
**8080** with `serve.ts`; CI smoke-tests it.

To test locally, run from the repository root:

```sh
docker build -f website/Dockerfile \
  --build-arg SITE_URL=https://docs.example.com \
  -t orch-website .
docker run --rm -p 127.0.0.1:8080:8080 orch-website
```

Or without Docker, from `website/`:

```sh
bun install --frozen-lockfile
bun run build:site
bun run start
```

Check `/`, `/quick-start/`, `/pkl/v1alpha1/Application.pkl`, and `/sitemap.xml`. Unknown paths return the
generated 404 page with HTTP status 404, rather than the homepage.
