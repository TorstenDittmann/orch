<script lang="ts">
	import Empty from '../components/Empty.svelte';
	import Filter from '../components/Filter.svelte';
	import Segmented from '../components/Segmented.svelte';
	import Status from '../components/Status.svelte';
	import { ago, label, short, tone } from '../lib/format';
	import { href } from '../lib/router.svelte';
	import { live } from '../lib/state.svelte';

	let query = $state('');
	let show = $state('all');

	const apps = $derived(live.snapshot?.applications ?? []);
	const runtimes = $derived(live.snapshot?.runtimes?.runtimes ?? []);
	const replicas = $derived.by(() => {
		const counts = new Map<string, { total: number; ready: number }>();
		for (const runtime of runtimes) {
			if (label(runtime.kind) !== 'service') continue;
			const entry = counts.get(runtime.application) ?? { total: 0, ready: 0 };
			entry.total++;
			if (tone(runtime.healthStatus) === 'ok' || label(runtime.allocationPhase) === 'running') entry.ready++;
			counts.set(runtime.application, entry);
		}
		return counts;
	});
	const unhealthy = $derived(apps.filter((app) => tone(app.executionStatus) !== 'ok'));
	const filtered = $derived(
		(show === 'attention' ? unhealthy : apps).filter((app) => !query || app.name.toLowerCase().includes(query.toLowerCase()))
	);
</script>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Applications</h1>
			<p>Desired state, rollout progress, and live replicas for every deployed application.</p>
		</div>
	</div>

	<div class="row toolbar">
		<Filter bind:value={query} placeholder="Filter applications" />
		<Segmented
			bind:value={show}
			options={[
				{ value: 'all', label: 'All', count: apps.length },
				{ value: 'attention', label: 'Needs attention', count: unhealthy.length }
			]}
		/>
	</div>

	<section class="panel">
		{#if filtered.length}
			<div class="table-wrap">
				<table class="data">
					<thead>
						<tr><th>Name</th><th>Status</th><th>Replicas</th><th>Generation</th><th>Revision</th><th>Updated</th><th>Message</th></tr>
					</thead>
					<tbody>
						{#each filtered as app (app.name)}
							{@const count = replicas.get(app.name)}
							<tr class="clickable" onclick={() => (location.hash = href('apps', app.name))}>
								<td class="primary-cell"><a href={href('apps', app.name)}>{app.name}</a></td>
								<td><Status value={app.executionStatus} pulse /></td>
								<td>{count ? `${count.ready}/${count.total} ready` : '—'}</td>
								<td class="mono">
									{app.activeGeneration}<span class="faint">/{app.generation}</span>
									{#if app.activeGeneration !== app.generation}<span class="chip rolling">rolling out</span>{/if}
								</td>
								<td class="mono faint">{short(app.revisionId)}</td>
								<td class="faint" title={app.updatedAt ?? ''}>{ago(app.updatedAt, live.now)}</td>
								<td class="wrap">{app.executionMessage || app.executionReason}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{:else if apps.length}
			<Empty title="No applications match" />
		{:else}
			<Empty title="No applications yet">Deploy one with <code>orch deploy app.pkl</code>.</Empty>
		{/if}
	</section>
</div>

<style>
	.toolbar {
		margin-bottom: 16px;
		justify-content: space-between;
	}
	.rolling {
		margin-left: 8px;
		background: var(--info-soft);
		color: var(--info);
		border-color: transparent;
	}
</style>
