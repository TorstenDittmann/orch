# orch website

A dependency-free static landing page for developers. It contains no client-side
JavaScript; the small worker only serves the static files in production.

Serve this directory with any static file server, for example:

```bash
python3 -m http.server 3000 --directory website
```

Run `./website/build.sh` before packaging a production deployment.
