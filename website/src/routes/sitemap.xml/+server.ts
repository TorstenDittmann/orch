import { pages, site } from "../../lib/server/docs";

export const prerender = true;

export function GET() {
	const urls = ["/", ...pages.map((page) => page.path)]
		.map((path) => `\t<url><loc>${site.url}${path}</loc></url>`)
		.join("\n");
	const body = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
${urls}
</urlset>
`;
	return new Response(body, { headers: { "content-type": "application/xml" } });
}
