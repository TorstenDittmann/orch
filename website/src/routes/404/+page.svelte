<script lang="ts">
	import Seo from "../../lib/components/Seo.svelte";
</script>

<Seo title="Page not found · orch" description="This page does not exist." />

<section class="container">
	<p class="code">404</p>
	<h1>This page does not exist.</h1>
	<p>It may have moved when the docs were reorganized.</p>
	<div class="links">
		<a href="/">Home</a>
		<a href="/docs/">Docs</a>
		<a href="/quick-start/">Quickstart</a>
	</div>
</section>

<style>
	section {
		padding-block: 96px 120px;
	}

	.code {
		margin: 0 0 8px;
		font-family: var(--mono);
		font-size: 14px;
		color: var(--dim);
	}

	h1 {
		margin: 0 0 12px;
		font-size: 34px;
		font-weight: 500;
		letter-spacing: -0.035em;
	}

	p {
		margin: 0 0 28px;
		color: var(--dim);
	}

	.links {
		display: flex;
		gap: 20px;
		font-size: 14px;
	}

	.links a {
		color: var(--accent);
	}
</style>
