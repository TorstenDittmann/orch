<script lang="ts">
	import Seo from "../../lib/components/Seo.svelte";

	let { data } = $props();

	let menu = $state<HTMLDetailsElement>();

	$effect(() => {
		// Close the mobile menu after navigating to another page.
		data.path;
		if (menu) menu.open = false;
	});
</script>

<Seo title="{data.doc.title} · orch" description={data.description} />

<div class="container docs">
	<details class="menu" bind:this={menu}>
		<summary>Menu</summary>
		{@render sidebar()}
	</details>
	<aside class="sidebar">
		{@render sidebar()}
	</aside>

	<article>
		<h1>{data.doc.title}</h1>
		<div class="prose">{@html data.doc.html}</div>

		<nav class="pager" aria-label="Pages">
			{#if data.previous}
				<a href={data.previous.path}><small>Previous</small>{data.previous.title}</a>
			{:else}
				<span></span>
			{/if}
			{#if data.next}
				<a class="next" href={data.next.path}><small>Next</small>{data.next.title}</a>
			{/if}
		</nav>
		<a class="edit" href="{data.site.repo}/blob/main/docs/{data.doc.file}">Edit this page on GitHub</a>
	</article>
</div>

{#snippet sidebar()}
	<nav aria-label="Documentation">
		{#each data.nav as group}
			{#if group.title}<b>{group.title}</b>{/if}
			{#each group.items as item}
				<a href={item.path} aria-current={item.path === data.path ? "page" : undefined}>{item.title}</a>
			{/each}
		{/each}
	</nav>
{/snippet}

<style>
	.docs {
		display: grid;
		grid-template-columns: 180px minmax(0, 1fr);
		gap: 56px;
		padding-block: 40px 72px;
	}

	.sidebar nav,
	.menu nav {
		display: grid;
		gap: 6px;
		align-content: start;
		font-size: 14px;
		color: var(--dim);
	}

	.sidebar {
		position: sticky;
		top: 24px;
		align-self: start;
	}

	nav b {
		margin-top: 14px;
		font-size: 12px;
		font-weight: 500;
		color: var(--fg);
	}

	nav a:hover,
	nav a[aria-current="page"] {
		color: var(--fg);
	}

	nav a[aria-current="page"] {
		font-weight: 500;
	}

	.menu {
		display: none;
	}

	h1 {
		margin: 0 0 20px;
		font-size: 34px;
		font-weight: 500;
		line-height: 1.15;
		letter-spacing: -0.035em;
		text-wrap: balance;
	}

	.prose {
		max-width: 70ch;
	}

	.prose :global(h2) {
		margin: 48px 0 12px;
		font-size: 22px;
		font-weight: 500;
		letter-spacing: -0.025em;
	}

	.prose :global(h3) {
		margin: 32px 0 8px;
		font-size: 17px;
		font-weight: 500;
	}

	.prose :global(:is(h2, h3) a:hover::after) {
		content: " #";
		color: var(--dim);
	}

	.prose :global(p),
	.prose :global(ul),
	.prose :global(ol) {
		margin: 0 0 16px;
		color: color-mix(in srgb, var(--fg) 82%, var(--bg));
	}

	.prose :global(li + li) {
		margin-top: 4px;
	}

	.prose :global(p a),
	.prose :global(li a),
	.prose :global(td a) {
		color: var(--accent);
	}

	.prose :global(p a:hover),
	.prose :global(li a:hover),
	.prose :global(td a:hover) {
		text-decoration: underline;
		text-underline-offset: 3px;
	}

	.prose :global(:not(pre) > code) {
		font-size: 0.86em;
		background: var(--code);
		border: 1px solid var(--line);
		border-radius: 4px;
		padding: 1px 5px;
	}

	.prose :global(pre) {
		margin: 0 0 20px;
	}

	.prose :global(blockquote) {
		margin: 0 0 20px;
		padding: 2px 0 2px 16px;
		border-left: 2px solid var(--accent);
	}

	.prose :global(blockquote p) {
		margin: 0;
		color: var(--dim);
	}

	.prose :global(.table) {
		margin: 0 0 20px;
		overflow-x: auto;
	}

	.prose :global(table) {
		border-collapse: collapse;
		width: 100%;
		font-size: 14px;
	}

	.prose :global(th),
	.prose :global(td) {
		text-align: left;
		vertical-align: top;
		padding: 8px 16px 8px 0;
		border-bottom: 1px solid var(--line);
	}

	.prose :global(th) {
		font-weight: 500;
	}

	.prose :global(hr) {
		border: 0;
		border-top: 1px solid var(--line);
		margin: 32px 0;
	}

	.pager {
		display: flex;
		justify-content: space-between;
		gap: 16px;
		margin-top: 56px;
		padding-top: 24px;
		border-top: 1px solid var(--line);
		max-width: 70ch;
	}

	.pager a {
		display: grid;
		font-weight: 500;
	}

	.pager small {
		font-size: 12px;
		font-weight: 400;
		color: var(--dim);
	}

	.pager .next {
		text-align: right;
	}

	.pager a:hover {
		color: var(--accent);
	}

	.edit {
		display: inline-block;
		margin-top: 24px;
		font-size: 13px;
		color: var(--dim);
	}

	.edit:hover {
		color: var(--fg);
	}

	@media (max-width: 760px) {
		.docs {
			grid-template-columns: minmax(0, 1fr);
			gap: 24px;
			padding-block: 8px 56px;
		}

		.sidebar {
			display: none;
		}

		.menu {
			display: block;
			border-block: 1px solid var(--line);
			padding-block: 10px;
		}

		.menu summary {
			cursor: pointer;
			font-size: 14px;
		}

		.menu nav {
			padding-block: 8px 6px;
		}

		h1 {
			font-size: 28px;
		}
	}
</style>
