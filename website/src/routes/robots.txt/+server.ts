import { site } from "../../lib/server/docs";

export const prerender = true;

export function GET() {
	return new Response(`User-agent: *\nAllow: /\n\nSitemap: ${site.url}/sitemap.xml\n`, {
		headers: { "content-type": "text/plain" },
	});
}
