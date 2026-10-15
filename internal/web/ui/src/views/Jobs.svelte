<script lang="ts">
	import { Ban, RefreshCw, RotateCw, ScrollText } from '@lucide/svelte';
	import { onDestroy } from 'svelte';
	import Empty from '../components/Empty.svelte';
	import Status from '../components/Status.svelte';
	import { api } from '../lib/api';
	import { ago, datetime, label, short } from '../lib/format';
	import { href } from '../lib/router.svelte';
	import { ask, live, openLogs } from '../lib/state.svelte';
	import type { OneOffJob } from '../lib/types';

	let app = $state('');
	let jobs = $state<OneOffJob[]>([]);
	let next = $state('');
	let error = $state('');
	let loading = $state(true);

	const apps = $derived(live.snapshot?.applications ?? []);
	let timer: ReturnType<typeof setTimeout> | undefined;

	async function load() {
		clearTimeout(timer);
		try {
			const response = await api.jobs(app);
			jobs = response.jobs ?? [];
			next = response.nextBeforeId ?? '';
			error = '';
		} catch (err) {
			error = (err as Error).message;
		} finally {
			loading = false;
		}
		timer = setTimeout(load, document.hidden ? 15000 : 4000);
	}

	async function more() {
		try {
			const response = await api.jobs(app, next);
			jobs = [...jobs, ...(response.jobs ?? [])];
			next = response.nextBeforeId ?? '';
		} catch (err) {
			error = (err as Error).message;
		}
	}

	$effect(() => {
		void app;
		loading = true;
		load();
	});
	onDestroy(() => clearTimeout(timer));

	const finished = (job: OneOffJob) => ['succeeded', 'failed', 'timed out', 'skipped'].includes(label(job.status?.state));

	function cancel(job: OneOffJob) {
		ask({ title: `Cancel job ${short(job.id, 8)}?`, body: `The ${job.service} job in ${job.application} is stopped.`, action: 'Cancel job', danger: true, success: 'Cancellation requested', run: () => api.jobAction(job.id, 'cancel').then(load) });
	}
	function retry(job: OneOffJob) {
		ask({ title: `Retry job ${short(job.id, 8)}?`, body: `A new run of the same command starts in ${job.application}.`, action: 'Retry', success: 'Retry started', run: () => api.jobAction(job.id, 'retry').then(load) });
	}
</script>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Jobs</h1>
			<p>One-off commands and deploy hooks run with <code>orch job run</code>.</p>
		</div>
		<div class="row">
			<select class="input" bind:value={app} aria-label="Application">
				<option value="">All applications</option>
				{#each apps as item (item.name)}<option value={item.name}>{item.name}</option>{/each}
			</select>
			<button class="btn icon" aria-label="Refresh" onclick={load}><RefreshCw size={14} /></button>
		</div>
	</div>

	<section class="panel">
		{#if error}
			<Empty title="Could not load jobs">{error}</Empty>
		{:else if loading && !jobs.length}
			<Empty title="Loading jobs…" />
		{:else if jobs.length}
			<div class="table-wrap">
				<table class="data">
					<thead><tr><th>Job</th><th>Application</th><th>Service</th><th>Kind</th><th>State</th><th class="num">Exit</th><th>Scheduled</th><th>Finished</th><th></th></tr></thead>
					<tbody>
						{#each jobs as job (job.id)}
							<tr>
								<td class="mono primary-cell" title={job.id}>{short(job.id, 10)}{#if job.retryOf}<span class="faint"> ↻</span>{/if}</td>
								<td><a class="link" href={href('apps', job.application)}>{job.application}</a></td>
								<td>{job.service}</td>
								<td><span class="chip">{job.kind || 'manual'}</span></td>
								<td><Status value={job.status?.state} pulse />{#if job.cancelRequested && !finished(job)}<span class="chip">cancelling</span>{/if}</td>
								<td class="num mono">{job.status?.hasExitCode ? job.status.exitCode : '—'}</td>
								<td class="faint" title={datetime(job.status?.scheduledAt)}>{ago(job.status?.scheduledAt, live.now)}</td>
								<td class="faint">{ago(job.status?.finishedAt, live.now)}</td>
								<td>
									<div class="row nowrap">
										<button class="btn sm" onclick={() => openLogs({ kind: 'run', app: job.application, job: job.status?.job || job.service, run: job.id, label: `job ${short(job.id, 8)}` })}><ScrollText size={13} /> Logs</button>
										{#if finished(job)}
											<button class="btn sm" onclick={() => retry(job)}><RotateCw size={13} /> Retry</button>
										{:else if !job.cancelRequested}
											<button class="btn sm" onclick={() => cancel(job)}><Ban size={13} /> Cancel</button>
										{/if}
									</div>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			{#if next}<div class="more"><button class="btn" onclick={more}>Load older jobs</button></div>{/if}
		{:else}
			<Empty title="No jobs yet">Run one with <code>orch job run APP SERVICE -- COMMAND</code>.</Empty>
		{/if}
	</section>
</div>

<style>
	.nowrap {
		flex-wrap: nowrap;
	}
	.more {
		display: flex;
		justify-content: center;
		padding: 14px;
		border-top: 1px solid var(--border);
	}
</style>
