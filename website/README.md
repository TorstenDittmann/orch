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

## Deploy to Dokploy

The Dockerfile uses Bun to build the site and serve `dist` on port **8080**
with the small static server in `serve.ts`. No Nginx is needed.

Create a Dokploy application connected to this repository and configure:

| Setting | Value |
| --- | --- |
| Repository build path | `/` (repository root) |
| Build type | Dockerfile |
| Dockerfile path | `website/Dockerfile` |
| Docker context path | `.` |
| Domain container port | `8080` |
| Domain path | `/` |

Keep the context at the repository root: the site reads Markdown from `docs/`
and schemas from `pkl/` as well as assets from `website/`.

Under **Environment → Build Time Arguments**, set the site's public origin:

```dotenv
SITE_URL=https://docs.example.com
```

Replace the example with your actual domain. This controls canonical links,
the sitemap, robots.txt, and generated documentation URLs. It is a **build-time**
value; changing a runtime environment variable does not update the static files.
Rebuild after changing it. Without an override, the existing Sites URL is used.

Point the domain's DNS at the Dokploy server, add the domain in Dokploy with
container port `8080`, and enable HTTPS with Let's Encrypt. Do not publish a
host port: Dokploy's proxy handles public HTTP/HTTPS traffic. Then deploy.

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
