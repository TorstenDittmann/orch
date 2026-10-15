// Hash routing keeps the Go server free of client route knowledge.
export interface Route {
	path: string;
	parts: string[];
	query: URLSearchParams;
}

function parse(): Route {
	const raw = location.hash.replace(/^#/, '') || '/';
	const [path, search = ''] = raw.split('?');
	return { path, parts: path.split('/').filter(Boolean).map(decodeURIComponent), query: new URLSearchParams(search) };
}

export const router = $state({ route: parse() });

window.addEventListener('hashchange', () => {
	router.route = parse();
	window.scrollTo({ top: 0 });
});

export function href(...parts: (string | number)[]): string {
	return '#/' + parts.map((part) => encodeURIComponent(String(part))).join('/');
}

export function navigate(target: string) {
	location.hash = target.replace(/^#/, '');
}
