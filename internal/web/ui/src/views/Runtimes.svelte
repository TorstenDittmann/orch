<script lang="ts">
	import { ScrollText } from '@lucide/svelte';
	import Empty from '../components/Empty.svelte';
	import Filter from '../components/Filter.svelte';
	import Segmented from '../components/Segmented.svelte';
	import Status from '../components/Status.svelte';
	import { ago, bytes, cores, label } from '../lib/format';
	import { href } from '../lib/router.svelte';
	import { live, openLogs } from '../lib/state.svelte';
	import type { Runtime } from '../lib/types';

	let query = $state('');
	let group = $state('application');
	let kind = $state('all');

	const runtimes = $derived(live.snapshot?.runtimes?.runtimes ?? []);
	const isRun = (runtime: Runtime) => label(runtime.kind) === 'cron run';
	const filtered = $derived(
		runtimes.filter((runtime) => {
			if (kind === 'service' && isRun(runtime)) return false;
			if (kind === 'run' && !isRun(runtime)) return false;
			if (!query) return true;
			return `${runtime.application} ${runtime.resource} ${runtime.nodeName}`.toLowerCase().includes(query.toLowerCase());
		})
	);
	const groups = $derived.by(() => {
		const map = new Map<string, Runtime[]>();
		for (const runtime of filtered) {
			const key = group === 'node' ? runtime.nodeName || 'Unplaced' : group === 'application' ? runtime.application : '';
			map.set(key, [...(map.get(key) ?? []), runtime]);
		}
		return [...map.entries()].sort(([a], [b]) => a.localeCompare(b));
	});

	function logs(runtime: Runtime) {
		openLogs(
			isRun(runtime)
				? { kind: 'run', app: runtime.application, job: runtime.resource, run: runtime.runId }
				: { kind: 'service', app: runtime.application, service: runtime.resource, replica: runtime.replica }
		);
	}
</script>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Runtimes</h1>
			<p>Every Orch-managed service replica and retained cron run. Select one to stream its logs.</p>
		</div>
	</div>

	<div class="row toolbar">
		<Filter bind:value={query} placeholder="Filter by app, service, node" />
		<div class="row">
			<Segmented
				bind:value={kind}
				options={[
					{ value: 'all', label: 'All' },
					{ value: 'service', label: 'Services' },
					{ value: 'run', label: 'Cron runs' }
				]}
			/>
			<Segmented
				bind:value={group}
				options={[
					{ value: 'application', label: 'By app' },
					{ value: 'node', label: 'By node' },
					{ value: 'none', label: 'Flat' }
				]}
			/>
		</div>
	</div>

	{#if live.snapshot?.runtimes?.truncated}
		<p class="faint">The runtime list was truncated by the controller.</p>
	{/if}

	{#if !filtered.length}
		<section class="panel"><Empty title={runtimes.length ? 'No runtimes match' : 'No runtimes yet'} /></section>
	{/if}

	<div class="stack">
		{#each groups as [name, items] (name)}
			<section class="panel">
				{#if name}
					<div class="panel-head">
						<h2>
							{#if group === 'application'}<a href={href('apps', name)}>{name}</a>{:else if name !== 'Unplaced'}<a href={href('nodes', name)}>{name}</a>{:else}{name}{/if}
						</h2>
						<span class="hint">{items.length}</span>
					</div>
				{/if}
				<div class="table-wrap">
					<table class="data">
						<thead>
							<tr>
								{#if group !== 'application'}<th>Application</th>{/if}
								<th>Resource</th><th>Kind</th><th>State</th><th>Health</th>
								{#if group !== 'node'}<th>Node</th>{/if}
								<th class="num">CPU</th><th class="num">Memory</th><th class="num">Restarts</th><th>Started</th><th>Message</th><th></th>
							</tr>
						</thead>
						<tbody>
							{#each items as runtime (runtime.id)}
								<tr class="clickable" onclick={() => logs(runtime)}>
									{#if group !== 'application'}<td>{runtime.application}</td>{/if}
									<td class="primary-cell">
										{runtime.resource}
										<span class="faint mono">{isRun(runtime) ? runtime.runId.slice(0, 8) : `#${runtime.replica}`}</span>
									</td>
									<td><span class="chip">{isRun(runtime) ? 'cron run' : 'service'}</span></td>
									<td><Status value={isRun(runtime) ? runtime.jobRunState : runtime.allocationPhase} /></td>
									<td>{#if !isRun(runtime)}<Status value={runtime.healthStatus} />{/if}</td>
									{#if group !== 'node'}<td>{runtime.nodeName || '—'}</td>{/if}
									<td class="num mono">{cores(runtime.cpuMillis)}</td>
									<td class="num mono">{bytes(runtime.memoryBytes)}</td>
									<td class="num">{runtime.restartCount}</td>
									<td class="faint">{ago(runtime.startedAt, live.now)}</td>
									<td class="wrap">{runtime.message || runtime.reason}{runtime.hasExitCode ? ` (exit ${runtime.exitCode})` : ''}</td>
									<td><span class="btn sm ghost"><ScrollText size={13} /> Logs</span></td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			</section>
		{/each}
	</div>
</div>

<style>
	.toolbar {
		margin-bottom: 16px;
		justify-content: space-between;
	}
	h2 a:hover {
		color: var(--accent-text);
	}
</style>
