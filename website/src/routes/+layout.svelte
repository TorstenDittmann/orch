<script lang="ts">
	import "@fontsource-variable/geist";
	import "@fontsource-variable/geist-mono";
	import "../app.css";
	import { page } from "$app/state";
	import { onMount } from "svelte";

	let { data, children } = $props();

	const isDocs = $derived(page.url.pathname !== "/");

	// Code blocks are server-rendered HTML, so their copy buttons are handled by delegation.
	onMount(() => {
		async function copy(event: MouseEvent) {
			const button = (event.target as Element).closest<HTMLButtonElement>("button.copy");
			if (!button) return;
			try {
				await navigator.clipboard.writeText(button.dataset.copy ?? "");
			} catch {
				return;
			}
			button.dataset.copied = "";
			button.ariaLabel = "Copied";
			setTimeout(() => {
				delete button.dataset.copied;
				button.ariaLabel = "Copy code";
			}, 1500);
		}
		document.addEventListener("click", copy);
		return () => document.removeEventListener("click", copy);
	});
</script>

<header class="container">
	<nav aria-label="Main">
		<a class="logo" href="/">orch</a>
		<a href="/docs/" aria-current={isDocs && page.url.pathname !== "/quick-start/" ? "page" : undefined}>Docs</a>
		<a href="/quick-start/" aria-current={page.url.pathname === "/quick-start/" ? "page" : undefined}>Quickstart</a>
		<a class="strong" href={data.site.repo}>GitHub</a>
	</nav>
</header>

<main>
	{@render children()}
</main>

<footer class="container">
	<div>
		<span>orch is in early development.</span>
		<a href={data.site.repo}>github.com/TorstenDittmann/orch</a>
	</div>
</footer>

<style>
	nav {
		display: flex;
		align-items: center;
		gap: 22px;
		padding-block: 22px;
		font-size: 14px;
		color: var(--dim);
	}

	nav a:hover,
	nav a[aria-current="page"] {
		color: var(--fg);
	}

	.logo {
		margin-right: auto;
		font-family: var(--mono);
		font-weight: 500;
		font-size: 15px;
		color: var(--fg);
	}

	.strong {
		color: var(--fg);
	}

	footer div {
		display: flex;
		flex-wrap: wrap;
		justify-content: space-between;
		gap: 8px 24px;
		border-top: 1px solid var(--line);
		padding-block: 22px 32px;
		font-size: 13px;
		color: var(--dim);
	}

	footer a:hover {
		color: var(--fg);
	}
</style>
