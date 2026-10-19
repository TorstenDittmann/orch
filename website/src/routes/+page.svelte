<script lang="ts">
	import Seo from "../lib/components/Seo.svelte";

	let { data } = $props();

	const comparison = [
		{ label: "Setup", swarm: "init and join", kubernetes: "control plane, network plugin, add‑ons", orch: "init and join" },
		{ label: "App definition", swarm: "Compose file", kubernetes: "many YAML resources", orch: "one typed file, checked before deploy" },
		{ label: "HTTPS", swarm: "bring your own proxy", kubernetes: "ingress controller and cert‑manager", orch: "built in" },
		{ label: "Private network", swarm: "overlay, encryption opt‑in", kubernetes: "depends on the network plugin", orch: "WireGuard mesh by default" },
		{ label: "Deploys", swarm: "rolling updates", kubernetes: "rolling updates, diff with kubectl", orch: "plan first, health‑checked rollout, automatic rollback" },
	];
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
			<colgroup>
				<col class="label" />
				<col class="swarm" />
				<col />
				<col class="ours" />
			</colgroup>
			<thead>
				<tr><td></td><th scope="col">Docker Swarm</th><th scope="col">Kubernetes</th><th scope="col">orch</th></tr>
			</thead>
			<tbody>
				{#each comparison as row}
					<tr>
						<th scope="row">{row.label}</th>
						<td data-label="Docker Swarm">{row.swarm}</td>
						<td data-label="Kubernetes">{row.kubernetes}</td>
						<td data-label="orch">{row.orch}</td>
					</tr>
				{/each}
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

	table {
		width: 100%;
		table-layout: fixed;
		border-collapse: collapse;
		font-size: 15px;
		line-height: 1.5;
	}

	col.label {
		width: 17%;
	}

	col.swarm {
		width: 23%;
	}

	col.ours {
		width: 29%;
	}

	th,
	td {
		text-align: left;
		vertical-align: top;
		padding: 16px 20px;
		border-bottom: 1px solid var(--line);
		text-wrap: pretty;
	}

	th:first-child {
		padding-left: 0;
	}

	thead th {
		padding-block: 12px;
		font-size: 13px;
		font-weight: 500;
		color: var(--dim);
		border-bottom-color: var(--fg);
	}

	thead td {
		border-bottom-color: var(--fg);
	}

	tbody th {
		font-weight: 500;
		color: var(--fg);
	}

	td {
		color: color-mix(in srgb, var(--fg) 72%, var(--bg));
	}

	/* the orch column reads as a highlighted panel */
	td:last-child,
	thead th:last-child {
		background: color-mix(in srgb, var(--accent) 9%, var(--bg));
		color: var(--fg);
		font-weight: 500;
	}

	thead th:last-child {
		color: var(--accent);
		border-radius: 8px 8px 0 0;
	}

	tbody tr:last-child td:last-child {
		border-radius: 0 0 8px 8px;
	}

	tbody tr:last-child > * {
		border-bottom: 0;
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

		/* stack each comparison row into a card on narrow screens */
		table,
		tbody,
		tr,
		th,
		td {
			display: block;
		}

		colgroup,
		thead {
			display: none;
		}

		tbody {
			display: grid;
			gap: 12px;
		}

		tbody tr {
			border: 1px solid var(--line);
			border-radius: 10px;
			overflow: hidden;
		}

		tbody th,
		tbody td,
		tbody tr:last-child > *,
		th:first-child {
			padding: 10px 16px;
			border-bottom: 1px solid var(--line);
		}

		tbody th {
			padding-block: 12px;
			font-size: 16px;
		}

		td::before {
			content: attr(data-label);
			display: block;
			font-size: 12px;
			font-weight: 500;
			color: var(--dim);
		}

		td:last-child,
		tbody tr:last-child td:last-child {
			border-bottom: 0;
			border-radius: 0;
		}

		td:last-child::before {
			color: var(--accent);
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
