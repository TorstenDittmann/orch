<script lang="ts">
	import { Copy, Plus } from '@lucide/svelte';
	import Empty from '../components/Empty.svelte';
	import { api } from '../lib/api';
	import { navigate } from '../lib/router.svelte';
	import { ask, loadClusters, panel, selectCluster, toast } from '../lib/state.svelte';

	let pending = $state<{ command: string; expiresAt: string } | null>(null);
	let creating = $state(false);

	// New clusters arrive from orch panel connect, so keep the list fresh.
	$effect(() => {
		loadClusters();
		const timer = setInterval(loadClusters, 3000);
		return () => clearInterval(timer);
	});

	async function connect() {
		creating = true;
		try {
			pending = await api.connectCode();
		} catch (error) {
			toast((error as Error).message, 'bad');
		} finally {
			creating = false;
		}
	}

	async function copy(text: string) {
		try {
			await navigator.clipboard.writeText(text);
			toast('Copied to clipboard', 'ok');
		} catch {
			toast('Copy failed; select the command instead', 'bad');
		}
	}

	function open(name: string) {
		selectCluster(name);
		navigate('/');
	}

	function disconnect(name: string) {
		ask({
			title: `Disconnect ${name}?`,
			body: 'The panel forgets this cluster and revokes its access token there. Apps on the cluster keep running. You can connect it again with a new code.',
			action: 'Disconnect',
			danger: true,
			success: `Disconnected ${name}`,
			run: async () => {
				await api.disconnect(name);
				await loadClusters();
			}
		});
	}
</script>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Clusters</h1>
			<p>Every orch cluster this panel manages.</p>
		</div>
		<button class="btn primary" onclick={connect} disabled={creating}><Plus size={14} /> Connect cluster</button>
	</div>

	{#if pending}
		<section class="panel connect">
			<div class="panel-head">
				<h2>Connect a cluster</h2>
				<button class="btn sm ghost" onclick={() => (pending = null)}>Done</button>
			</div>
			<div class="panel-body">
				<p class="muted">Run this on a workstation with administrator access to the cluster. The code works once, within 15 minutes, and the cluster shows up here when it is connected.</p>
				<div class="command">
					<code class="mono">{pending.command}</code>
					<button class="btn sm" onclick={() => copy(pending!.command)}><Copy size={13} /> Copy</button>
				</div>
			</div>
		</section>
	{/if}

	<section class="panel">
		{#if panel.clusters.length}
			<div class="table-wrap">
				<table class="data">
					<thead>
						<tr><th>Name</th><th>Controller</th><th>Status</th><th></th></tr>
					</thead>
					<tbody>
						{#each panel.clusters as cluster (cluster.name)}
							<tr class="clickable" class:selected={cluster.name === panel.current} onclick={() => open(cluster.name)}>
								<td class="primary-cell">{cluster.name}</td>
								<td class="mono faint">{cluster.endpoint}</td>
								<td class="wrap">
									{#if cluster.error}<span class="bad">{cluster.error}</span>{:else}<span class="chip">connected</span>{/if}
								</td>
								<td class="num">
									<button class="btn sm ghost" onclick={(event) => { event.stopPropagation(); disconnect(cluster.name); }}>Disconnect</button>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{:else}
			<div class="panel-body">
				<Empty title="No cluster is connected">Connect a cluster to manage it here.</Empty>
			</div>
		{/if}
	</section>
</div>

<style>
	.connect {
		margin-bottom: 16px;
	}
	.connect p {
		margin: 0 0 12px;
	}
	.command {
		display: flex;
		align-items: center;
		gap: 10px;
		padding: 10px 12px;
		border-radius: var(--radius-sm);
		background: var(--surface-2);
		border: 1px solid var(--border);
	}
	.command code {
		flex: 1;
		min-width: 0;
		overflow-wrap: anywhere;
		font-size: 13px;
	}
	.bad {
		color: var(--bad);
	}
</style>
