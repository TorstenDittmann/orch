// Serves the prerendered site in dist/ on port 8080 for the Docker image.
import { resolve, sep } from "node:path";

const root = resolve(import.meta.dir, "dist");
const port = Number(process.env.PORT || 8080);
const notFound = Bun.file(resolve(root, "404.html"));

function fileFor(pathname: string): string | null {
	let decoded: string;
	try {
		decoded = decodeURIComponent(pathname);
	} catch {
		return null;
	}
	const path = resolve(root, `.${decoded}`);
	// Stay inside dist/ and keep the Sites worker private.
	if (path !== root && !path.startsWith(root + sep)) return null;
	if (path.startsWith(resolve(root, "server") + sep)) return null;
	return path;
}

Bun.serve({
	port,
	hostname: process.env.HOST || "0.0.0.0",
	async fetch(request) {
		const url = new URL(request.url);
		const path = fileFor(url.pathname);
		if (path) {
			const file = Bun.file(path);
			if (await file.exists()) {
				const cache = url.pathname.startsWith("/_app/immutable/") ? "public, max-age=31536000, immutable" : "public, max-age=300";
				const headers: Record<string, string> = { "cache-control": cache };
				// Pkl schemas are plain text, so browsers show them instead of downloading.
				if (path.endsWith(".pkl")) headers["content-type"] = "text/plain; charset=utf-8";
				return new Response(file, { headers });
			}
			if (await Bun.file(resolve(path, "index.html")).exists()) {
				if (!url.pathname.endsWith("/")) {
					return Response.redirect(`${url.pathname}/${url.search}`, 308);
				}
				return new Response(Bun.file(resolve(path, "index.html")), { headers: { "cache-control": "public, max-age=300" } });
			}
		}
		return new Response(notFound, { status: 404, headers: { "content-type": "text/html; charset=utf-8" } });
	},
});

console.log(`orch website listening on :${port}`);
