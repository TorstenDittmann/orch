import { job, run } from '@change.codes/ci';

export default job({
	name: 'Website checks',
	steps: [
		run('Install pinned toolchain', 'mise install'),
		run('Install pinned website dependencies', 'cd website && mise exec -- bun install --frozen-lockfile'),
		run('Check Docia navigation and links', 'cd website && mise exec -- bun run check'),
		run('Build production website', 'cd website && mise exec -- ./build.sh'),
		run(
			'Verify generated website contract',
			'test -f website/dist/index.html && test -f website/dist/quick-start/index.html && test -f website/dist/404.html && test -f website/dist/sitemap.xml && test -f website/dist/robots.txt && test -f website/dist/llms.txt && test -f website/dist/server/index.js',
		),
		run('Publish website preview', 'cd website && mise exec -- bunx @dittmann/yeet ./dist'),
	],
});
