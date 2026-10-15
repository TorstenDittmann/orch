<script lang="ts">
	import { page } from "$app/state";

	let { title, description }: { title: string; description: string } = $props();

	const site = $derived(page.data.site);
	const canonical = $derived(site.url + page.url.pathname);
</script>

<svelte:head>
	<title>{title}</title>
	<meta name="description" content={description} />
	<link rel="canonical" href={canonical} />
	<meta property="og:type" content="website" />
	<meta property="og:site_name" content="orch" />
	<meta property="og:title" content={title} />
	<meta property="og:description" content={description} />
	<meta property="og:url" content={canonical} />
	<meta property="og:image" content="{site.url}/og.png" />
	<meta name="twitter:card" content="summary_large_image" />
</svelte:head>
