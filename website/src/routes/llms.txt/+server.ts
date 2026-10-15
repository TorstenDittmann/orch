import { nav, site } from "../../lib/server/docs";

export const prerender = true;

export function GET() {
	const sections = nav
		.map((group) => {
			const links = group.items.map((item) => `- [${item.title}](${site.url}${item.path})`).join("\n");
			return group.title ? `## ${group.title}\n\n${links}` : links;
		})
		.join("\n\n");
	return new Response(`# ${site.title}\n\n> ${site.description}\n\n${sections}\n`, {
		headers: { "content-type": "text/plain" },
	});
}
