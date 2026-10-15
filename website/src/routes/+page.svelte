<script lang="ts">
	import Seo from "../lib/components/Seo.svelte";

	let { data } = $props();
</script>

<Seo title="orch: deploy containers to your own servers" description={data.site.description} />

<section class="container hero">
	<div class="intro">
		<h1>Deploy to your own servers.</h1>
		<p>The simplicity of Docker Swarm with the modern features of Kubernetes. Three commands to a cluster, one file per app.</p>
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

<section class="container compare">
	<h2>Between Swarm and Kubernetes</h2>
	<p class="sub">Swarm is easy but stopped growing. Kubernetes can do everything but needs a team to run it. orch keeps the easy part and adds what modern apps expect.</p>
	<div class="table">
		<table>
			<thead>
				<tr><th></th><th>Docker Swarm</th><th>Kubernetes</th><th>orch</th></tr>
			</thead>
			<tbody>
				<tr><th>Setup</th><td>init and join</td><td>control plane, network plugin, add-ons</td><td>init and join</td></tr>
				<tr><th>App definition</th><td>Compose file</td><td>many YAML resources</td><td>one typed file, checked before deploy</td></tr>
				<tr><th>HTTPS</th><td>bring your own proxy</td><td>ingress controller and cert-manager</td><td>built in</td></tr>
				<tr><th>Private network</th><td>overlay, encryption opt-in</td><td>depends on the network plugin</td><td>WireGuard mesh by default</td></tr>
				<tr><th>Deploys</th><td>rolling updates</td><td>rolling updates, diff with kubectl</td><td>plan first, health-checked rollout, automatic rollback</td></tr>
			</tbody>
		</table>
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

	.compare {
		padding-block: 88px 0;
	}

	.compare h2,
	.setup h2 {
		margin: 0 0 12px;
		font-size: 24px;
		font-weight: 500;
		letter-spacing: -0.03em;
	}

	.sub {
		margin: 0 0 32px;
		max-width: 60ch;
		color: var(--dim);
	}

	.table {
		overflow-x: auto;
	}

	table {
		width: 100%;
		min-width: 620px;
		border-collapse: collapse;
		font-size: 14px;
	}

	th,
	td {
		text-align: left;
		vertical-align: top;
		padding: 12px 16px 12px 0;
		border-bottom: 1px solid var(--line);
	}

	thead th {
		font-weight: 500;
		color: var(--dim);
		font-size: 13px;
	}

	tbody th {
		font-weight: 500;
		width: 18%;
	}

	td {
		color: var(--dim);
	}

	td:last-child,
	thead th:last-child {
		color: var(--fg);
		font-weight: 500;
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

		.compare {
			padding-block: 64px 0;
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
