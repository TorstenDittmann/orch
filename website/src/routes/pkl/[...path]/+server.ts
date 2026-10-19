import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { error } from "@sveltejs/kit";

// Publishes the versioned schemas from ../pkl so manifests can amend
// https://<site>/pkl/v1alpha1/Application.pkl. Only v* directories are public.
const pklDir = resolve(process.cwd(), "../pkl");

function schemas(): string[] {
	return readdirSync(pklDir, { withFileTypes: true })
		.filter((entry) => entry.isDirectory() && /^v\d/.test(entry.name))
		.flatMap((dir) =>
			readdirSync(resolve(pklDir, dir.name))
				.filter((file) => file.endsWith(".pkl"))
				.map((file) => `${dir.name}/${file}`),
		);
}

export const prerender = true;

export function entries() {
	return schemas().map((path) => ({ path }));
}

export function GET({ params }) {
	if (!schemas().includes(params.path)) error(404, "Not found");
	return new Response(readFileSync(resolve(pklDir, params.path), "utf8"), {
		headers: { "content-type": "text/plain; charset=utf-8" },
	});
}
