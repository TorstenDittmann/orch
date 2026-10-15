<script lang="ts">
	import Seo from "../lib/components/Seo.svelte";

	let { data } = $props();
</script>

<Seo title="orch: deploy containers to your own servers" description={data.site.description} />

<section class="container hero">
	<div class="intro">
		<h1>Deploy to your own servers.</h1>
		<p>A container orchestrator for 1 to 20 Linux nodes. Three commands to a cluster, one file per app.</p>
		<div class="actions">
			<a class="button" href="/quick-start/">Get started</a>
			<a class="quiet" href="/docs/">Read the docs</a>
		</div>
	</div>
	<div class="code">{@html data.manifest}</div>
</section>

<section class="container">
	<div class="features">
		<div>
			<h2>Simple setup</h2>
			<p>init and join. Your SSH key is the credential.</p>
		</div>
		<div>
			<h2>Safe deploys</h2>
			<p>Health checks, surge and automatic rollback.</p>
		</div>
		<div>
			<h2>Built-in networking</h2>
			<p>HTTPS ingress and an encrypted WireGuard mesh.</p>
		</div>
	</div>
</section>

<section class="container setup">
	<div class="intro">
		<h2>A cluster in three commands</h2>
		<p>In the style of Docker Swarm. There are no certificate bundles to copy between machines.</p>
		<a class="quiet" href="/installation/">Installation guide →</a>
	</div>
	<div class="code">{@html data.setup}</div>
</section>

<style>
	.hero,
	.setup {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 56px;
		align-items: center;
	}

	.hero {
		padding-block: 80px 72px;
	}

	.setup {
		padding-block: 72px 88px;
	}

	.hero > *,
	.setup > * {
		min-width: 0;
	}

	h1 {
		margin: 0 0 16px;
		font-size: clamp(34px, 5vw, 44px);
		font-weight: 500;
		line-height: 1.08;
		letter-spacing: -0.04em;
		text-wrap: balance;
	}

	.intro p {
		margin: 0 0 28px;
		max-width: 42ch;
		color: var(--dim);
	}

	.setup h2 {
		margin: 0 0 12px;
		font-size: 24px;
		font-weight: 500;
		letter-spacing: -0.03em;
	}

	.actions {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 12px 20px;
	}

	.button {
		background: var(--fg);
		color: var(--bg);
		padding: 9px 16px;
		border-radius: 6px;
		font-size: 14px;
		font-weight: 500;
	}

	.button:hover {
		opacity: 0.86;
	}

	.quiet {
		font-size: 14px;
		color: var(--dim);
	}

	.quiet:hover {
		color: var(--fg);
	}

	.features {
		display: grid;
		grid-template-columns: repeat(3, 1fr);
		border-block: 1px solid var(--line);
	}

	.features div {
		padding: 28px 24px 28px 0;
		min-width: 0;
	}

	.features div + div {
		padding-left: 24px;
		border-left: 1px solid var(--line);
	}

	.features h2 {
		margin: 0 0 6px;
		font-size: 16px;
		font-weight: 500;
	}

	.features p {
		margin: 0;
		font-size: 14px;
		color: var(--dim);
	}

	@media (max-width: 760px) {
		.hero,
		.setup {
			grid-template-columns: 1fr;
			gap: 36px;
		}

		.hero {
			padding-block: 48px;
		}

		.setup {
			padding-block: 48px 64px;
		}

		.features {
			grid-template-columns: 1fr;
		}

		.features div,
		.features div + div {
			padding: 22px 0;
			border-left: 0;
		}

		.features div + div {
			border-top: 1px solid var(--line);
		}
	}
</style>
