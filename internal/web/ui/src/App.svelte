<script lang="ts">
	import { Menu, RefreshCw, TriangleAlert } from '@lucide/svelte';
	import CommandPalette from './components/CommandPalette.svelte';
	import ConfirmDialog from './components/ConfirmDialog.svelte';
	import LogDrawer from './components/LogDrawer.svelte';
	import Sidebar from './components/Sidebar.svelte';
	import Toasts from './components/Toasts.svelte';
	import { router } from './lib/router.svelte';
	import { live, loadClusters, panel, refresh, ui } from './lib/state.svelte';
	import ApplicationDetail from './views/ApplicationDetail.svelte';
	import Clusters from './views/Clusters.svelte';
	import Applications from './views/Applications.svelte';
	import Events from './views/Events.svelte';
	import Jobs from './views/Jobs.svelte';
	import NodeDetail from './views/NodeDetail.svelte';
	import Nodes from './views/Nodes.svelte';
	import Overview from './views/Overview.svelte';
	import Runtimes from './views/Runtimes.svelte';

	loadClusters().then(refresh);

	const parts = $derived(router.route.parts);
	const partial = $derived(live.snapshot?.errors ?? []);

	$effect(() => {
		const name = live.snapshot?.cluster?.name;
		document.title = name ? `${name} · orch` : 'orch';
	});
</script>

<Sidebar />

<div class="shell">
	<div class="mobilebar">
		<button class="btn icon ghost" aria-label="Open menu" onclick={() => (ui.sidebar = true)}><Menu size={18} /></button>
		<span class="name">{live.snapshot?.cluster?.name || 'orch'}</span>
	</div>

	{#if live.unauthorized}
		<div class="gate">
			<div class="panel card">
				<h1>Not signed in</h1>
				<p class="muted">Open this page with the link <code>orch web</code> or <code>orch panel open</code> printed in your terminal. Each link works once, so run the command again for a new one.</p>
			</div>
		</div>
	{:else}
		{#if live.error && live.snapshot}
			<div class="banner bad">
				<TriangleAlert size={16} />
				<span>Lost connection to orch web: {live.error}. Showing the last snapshot.</span>
				<button class="btn sm" onclick={refresh}><RefreshCw size={13} /> Retry</button>
			</div>
		{:else if partial.length}
			<div class="banner warn">
				<TriangleAlert size={16} />
				<span>Some data could not be loaded: {partial.join(' · ')}</span>
			</div>
		{/if}

		{#if parts[0] === 'clusters' || (panel.enabled && !panel.clusters.length)}
			<Clusters />
		{:else if !live.snapshot}
			<div class="gate">
				{#if live.error}
					<div class="panel card">
						<h1>Cannot reach the controller</h1>
						<p class="muted">{live.error}</p>
						<button class="btn" onclick={refresh}><RefreshCw size={14} /> Try again</button>
					</div>
				{:else}
					<div class="spinner" aria-label="Loading"></div>
				{/if}
			</div>
		{:else if parts[0] === 'apps' && parts[1]}
			{#key parts[1]}<ApplicationDetail name={parts[1]} tab={parts[2] ?? 'overview'} />{/key}
		{:else if parts[0] === 'apps'}
			<Applications />
		{:else if parts[0] === 'runtimes'}
			<Runtimes />
		{:else if parts[0] === 'nodes' && parts[1]}
			<NodeDetail name={parts[1]} />
		{:else if parts[0] === 'nodes'}
			<Nodes />
		{:else if parts[0] === 'jobs'}
			<Jobs />
		{:else if parts[0] === 'events'}
			<Events />
		{:else}
			<Overview />
		{/if}
	{/if}
</div>

<LogDrawer />
<CommandPalette />
<ConfirmDialog />
<Toasts />

<style>
	.shell {
		margin-left: var(--sidebar);
		min-height: 100vh;
	}
	.mobilebar {
		display: none;
	}
	.banner {
		display: flex;
		align-items: center;
		gap: 10px;
		padding: 10px 32px;
		font-size: 13.5px;
		border-bottom: 1px solid var(--border);
	}
	.banner span {
		flex: 1;
	}
	.banner.bad {
		background: var(--bad-soft);
		color: var(--bad);
	}
	.banner.warn {
		background: var(--warn-soft);
		color: var(--warn);
	}
	.gate {
		min-height: 70vh;
		display: grid;
		place-items: center;
		padding: 24px;
	}
	.card {
		padding: 28px;
		max-width: 460px;
	}
	.card h1 {
		font-size: 18px;
		margin-bottom: 8px;
	}
	.card p {
		margin: 0 0 16px;
	}
	.spinner {
		width: 28px;
		height: 28px;
		border-radius: 50%;
		border: 3px solid var(--surface-3);
		border-top-color: var(--accent);
		animation: spin 0.8s linear infinite;
	}
	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}
	@media (max-width: 900px) {
		.shell {
			margin-left: 0;
		}
		.mobilebar {
			display: flex;
			align-items: center;
			gap: 8px;
			padding: 8px 12px;
			border-bottom: 1px solid var(--border);
			background: var(--surface);
			position: sticky;
			top: 0;
			z-index: 20;
		}
		.name {
			font-weight: 600;
		}
		.banner {
			padding: 10px 16px;
		}
	}
</style>
