import { svelte } from '@sveltejs/vite-plugin-svelte';
import { defineConfig } from 'vite';

// `bun run dev` proxies the API to a running `orch web --port 7070 --no-open`.
// Open the printed token URL on port 7070 once so the session cookie exists.
export default defineConfig({
	plugins: [svelte()],
	base: '/',
	build: { outDir: 'dist', emptyOutDir: true, assetsInlineLimit: 0, chunkSizeWarningLimit: 800 },
	server: { proxy: { '/api': 'http://127.0.0.1:7070' } }
});
