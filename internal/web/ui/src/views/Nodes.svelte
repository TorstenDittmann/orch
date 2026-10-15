<script lang="ts">
	import { EllipsisVertical } from '@lucide/svelte';
	import Empty from '../components/Empty.svelte';
	import Filter from '../components/Filter.svelte';
	import Meter from '../components/Meter.svelte';
	import Status from '../components/Status.svelte';
	import { nodeActions } from '../lib/actions';
	import { ago, bytes, cores, ratio } from '../lib/format';
	import { href } from '../lib/router.svelte';
	import { live } from '../lib/state.svelte';

	let query = $state('');
	let menu = $state('');

	const nodes = $derived(live.snapshot?.nodes ?? []);
	const filtered = $derived(
		nodes.filter((node) => !query || `${node.name} ${node.advertiseAddress} ${Object.entries(node.labels ?? {}).flat().join(' ')}`.toLowerCase().includes(query.toLowerCase()))
	);
</script>

<svelte:window onclick={() => (menu = '')} />

<div class="page">
	<div class="page-head">
		<div>
			<h1>Nodes</h1>
			<p>Capacity, connectivity, and scheduling state of every worker.</p>
		</div>
	</div>

	<div class="row toolbar"><Filter bind:value={query} placeholder="Filter by name, address, label" /></div>

	{#if filtered.length}
		<div class="cards">
			{#each filtered as node (node.name)}
				<article class="panel card">
					<header>
						<a class="name" href={href('nodes', node.name)}>{node.name}</a>
						<div class="menu-wrap">
							<button class="btn icon ghost sm" aria-label="Actions for {node.name}" onclick={(event) => { event.stopPropagation(); menu = menu === node.name ? '' : node.name; }}>
								<EllipsisVertical size={16} />
							</button>
							{#if menu === node.name}
								<div class="menu" role="menu">
									{#each nodeActions(node) as action (action.id)}
										<button role="menuitem" class:danger={action.danger} onclick={action.run}>{action.label}</button>
									{/each}
									<a role="menuitem" href={href('nodes', node.name)}>View details</a>
								</div>
							{/if}
						</div>
					</header>
					<div class="row badges">
						<Status value={node.connectionStatus} pulse />
						<Status value={node.lifecycle} />
						{#if node.ingress}<span class="chip">ingress</span>{/if}
					</div>
					<div class="meters">
						<Meter value={ratio(node.assignedCpuMillis, node.allocatableCpuMillis)} label="CPU" detail="{cores(node.assignedCpuMillis)} / {cores(node.allocatableCpuMillis)} cores" />
						<Meter value={ratio(node.assignedMemoryBytes, node.allocatableMemoryBytes)} label="Memory" detail="{bytes(node.assignedMemoryBytes)} / {bytes(node.allocatableMemoryBytes)}" />
					</div>
					<dl class="facts">
						<div><dt>Address</dt><dd class="mono">{node.advertiseAddress || '—'}</dd></div>
						<div><dt>Platform</dt><dd>{node.operatingSystem || '?'}/{node.architecture || '?'}</dd></div>
						<div><dt>Last seen</dt><dd>{ago(node.lastSeenAt, live.now)}</dd></div>
					</dl>
					{#if Object.keys(node.labels ?? {}).length}
						<div class="labels">
							{#each Object.entries(node.labels) as [key, value] (key)}<span class="chip mono">{key}={value}</span>{/each}
						</div>
					{/if}
					{#if node.drainBlockers?.length}
						<div class="blockers">Drain blocked by {node.drainBlockers.join(', ')}</div>
					{/if}
				</article>
			{/each}
		</div>
	{:else}
		<section class="panel"><Empty title={nodes.length ? 'No nodes match' : 'No nodes yet'}>{#if !nodes.length}Join one with <code>orch node join create</code>.{/if}</Empty></section>
	{/if}
</div>

<style>
	.toolbar {
		margin-bottom: 16px;
	}
	.cards {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
		gap: 16px;
	}
	.card {
		padding: 16px;
		display: flex;
		flex-direction: column;
		gap: 14px;
	}
	header {
		display: flex;
		justify-content: space-between;
		align-items: center;
	}
	.name {
		font-weight: 600;
		font-size: 15px;
	}
	.name:hover {
		color: var(--accent-text);
	}
	.badges {
		gap: 6px;
	}
	.meters {
		display: grid;
		gap: 12px;
	}
	.facts {
		display: grid;
		grid-template-columns: repeat(3, minmax(0, 1fr));
		gap: 8px;
		margin: 0;
		padding-top: 12px;
		border-top: 1px solid var(--border);
	}
	dt {
		font-size: 12px;
		color: var(--text-3);
	}
	dd {
		margin: 0;
		font-size: 13px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.labels {
		display: flex;
		flex-wrap: wrap;
		gap: 6px;
	}
	.blockers {
		font-size: 13px;
		color: var(--warn);
		background: var(--warn-soft);
		padding: 8px 10px;
		border-radius: var(--radius-sm);
	}
	.menu-wrap {
		position: relative;
	}
	.menu {
		position: absolute;
		right: 0;
		top: 34px;
		min-width: 180px;
		background: var(--surface);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: var(--shadow-lg);
		padding: 4px;
		z-index: 10;
		display: flex;
		flex-direction: column;
	}
	.menu button,
	.menu a {
		text-align: left;
		border: 0;
		background: none;
		padding: 8px 10px;
		border-radius: var(--radius-sm);
		cursor: pointer;
		font-size: 13.5px;
	}
	.menu button:hover,
	.menu a:hover {
		background: var(--surface-2);
	}
	.menu .danger {
		color: var(--bad);
	}
</style>
