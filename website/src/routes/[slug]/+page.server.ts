import { error } from "@sveltejs/kit";
import { nav, pages, plainDescription, renderDoc } from "../../lib/server/docs";

export function entries() {
	return pages.map((page) => ({ slug: page.path.slice(1, -1) }));
}

export function load({ params }) {
	const index = pages.findIndex((page) => page.path === `/${params.slug}/`);
	if (index === -1) error(404, "Not found");
	const doc = renderDoc(pages[index].file);
	return {
		nav,
		doc,
		path: pages[index].path,
		description: plainDescription(doc),
		previous: pages[index - 1] ?? null,
		next: pages[index + 1] ?? null,
	};
}
