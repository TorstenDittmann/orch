import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { Marked, type Tokens } from "marked";
import { createCssVariablesTheme, createHighlighter } from "shiki";

const docsDir = resolve(process.cwd(), "../docs");

export const site = {
	title: "orch",
	description: "Container orchestration for the agentic era. Deploy to your own servers with the simplicity of Docker Swarm and the modern features of Kubernetes.",
	url: (process.env.SITE_URL || "https://orch.torsten.work").replace(/\/+$/, ""),
	repo: "https://github.com/TorstenDittmann/orch",
};

export type NavItem = { title: string; file: string; path: string };
export type NavGroup = { title: string | null; items: NavItem[] };

/** URL path for a docs Markdown file: README.md is the docs overview. */
function pathFor(file: string): string {
	return file === "README.md" ? "/docs/" : `/${file.replace(/\.md$/, "")}/`;
}

/** Sidebar order and titles come from docs/SUMMARY.md. */
function readSummary(): NavGroup[] {
	const groups: NavGroup[] = [{ title: null, items: [] }];
	for (const line of readFileSync(resolve(docsDir, "SUMMARY.md"), "utf8").split("\n")) {
		const link = line.match(/^\s*- \[(.+?)\]\((.+?\.md)\)/);
		if (link) {
			groups[groups.length - 1].items.push({ title: link[1], file: link[2], path: pathFor(link[2]) });
			continue;
		}
		const heading = line.match(/^- (.+)$/);
		if (heading) groups.push({ title: heading[1].trim(), items: [] });
	}
	return groups.filter((group) => group.items.length > 0);
}

export const nav = readSummary();
export const pages = nav.flatMap((group) => group.items);

// Token colors are CSS variables defined in app.css, so code follows the site theme.
const theme = createCssVariablesTheme({ name: "orch", variablePrefix: "--code-" });
// Bare shell arguments read as plain text; otherwise whole command lines turn string-colored.
theme.tokenColors!.push({ scope: "string.unquoted.argument", settings: { foreground: "var(--code-foreground)" } });

const highlighter = await createHighlighter({
	themes: [theme],
	langs: ["pkl", "shellscript", "yaml", "json", "text"],
});

function escapeHtml(text: string): string {
	return text.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
}

/**
 * Highlights a console transcript: "$ " lines as shell commands, the rest as
 * dimmed output. Shiki's console grammar leaves the commands uncolored.
 */
function consoleHtml(code: string): string {
	const lines = code.split("\n").map((line) => {
		if (!line.startsWith("$ ")) return `<span class="line"><span class="output">${escapeHtml(line)}</span></span>`;
		const [tokens] = highlighter.codeToTokens(line.slice(2), { lang: "shellscript", theme: "orch" }).tokens;
		const spans = tokens.map((token) => `<span style="color:${token.color}">${escapeHtml(token.content)}</span>`);
		return `<span class="line"><span class="prompt">$ </span>${spans.join("")}</span>`;
	});
	return `<pre class="shiki orch" tabindex="0"><code>${lines.join("\n")}</code></pre>`;
}

/**
 * Highlights a code block and wraps it with a copy button. Console blocks
 * without "$ " prompts are plain commands, so they are highlighted as shell;
 * with prompts, only the commands are copied.
 */
function codeBlock(code: string, lang: string): string {
	const prompts = lang === "console" && /^\$ /m.test(code);
	let html: string;
	let copy = code;
	if (prompts) {
		html = consoleHtml(code);
		copy = code
			.split("\n")
			.filter((line) => line.startsWith("$ "))
			.map((line) => line.slice(2))
			.join("\n");
	} else {
		let language = lang === "console" ? "shellscript" : lang;
		if (!highlighter.getLoadedLanguages().includes(language)) language = "text";
		html = highlighter.codeToHtml(code, { lang: language, theme: "orch" });
	}
	return `<div class="codeblock">${html}<button type="button" class="copy" data-copy="${escapeHtml(copy)}" aria-label="Copy code"></button></div>`;
}

function slugify(text: string): string {
	return text
		.toLowerCase()
		.replace(/<[^>]+>/g, "")
		.replace(/[^\p{L}\p{N}\s-]/gu, "")
		.trim()
		.replace(/\s/g, "-");
}

/** Rewrites links between Markdown files to site paths and fails on unknown files. */
function rewriteHref(href: string, from: string): string {
	if (/^[a-z]+:/i.test(href) || href.startsWith("#")) return href;
	const [file, hash] = href.split("#");
	const target = pages.find((page) => page.file === file);
	if (!target) throw new Error(`${from}: link to ${href} does not match a page in SUMMARY.md`);
	return target.path + (hash ? `#${hash}` : "");
}

export type Heading = { depth: number; text: string; id: string };
export type Doc = { title: string; html: string; headings: Heading[]; file: string };

export function renderDoc(file: string): Doc {
	const source = readFileSync(resolve(docsDir, file), "utf8");
	const seen = new Map<string, number>();
	const headings: Heading[] = [];
	let title = "";

	const marked = new Marked({
		gfm: true,
		walkTokens(token) {
			if (token.type === "link") (token as Tokens.Link).href = rewriteHref((token as Tokens.Link).href, file);
		},
		renderer: {
			heading({ tokens, depth, text }) {
				const html = this.parser.parseInline(tokens);
				if (depth === 1) {
					title ||= text;
					return "";
				}
				let id = slugify(text);
				const count = seen.get(id) ?? 0;
				seen.set(id, count + 1);
				if (count) id = `${id}-${count}`;
				headings.push({ depth, text: html.replace(/<[^>]+>/g, ""), id });
				return `<h${depth} id="${id}"><a href="#${id}">${html}</a></h${depth}>\n`;
			},
			code({ text, lang }) {
				return codeBlock(text, lang || "text");
			},
		},
	});

	const html = marked
		.parse(source, { async: false })
		.replaceAll("<table>", '<div class="table"><table>')
		.replaceAll("</table>", "</table></div>");
	return { title: title || file, html, headings: headings.filter((h) => h.depth === 2), file };
}

/** First paragraph as plain text, for meta descriptions. */
export function plainDescription(doc: Doc): string {
	const paragraph = doc.html.match(/<p>([\s\S]*?)<\/p>/);
	if (!paragraph) return site.description;
	return paragraph[1]
		.replace(/<[^>]+>/g, "")
		.replace(/\s+/g, " ")
		.replace(/&quot;/g, '"')
		.replace(/&#39;/g, "'")
		.replace(/&lt;/g, "<")
		.replace(/&gt;/g, ">")
		.replace(/&amp;/g, "&")
		.trim();
}

/** Highlights a standalone snippet, such as the manifest on the landing page. */
export function highlight(code: string, lang: string): string {
	return codeBlock(code, lang);
}
