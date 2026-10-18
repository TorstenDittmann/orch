<script lang="ts">
	import { ArrowRight, ChevronRight, Copy, ExternalLink, GitCompare, History, RotateCcw, ScrollText } from '@lucide/svelte';
	import { onDestroy } from 'svelte';
	import Empty from '../components/Empty.svelte';
	import Status from '../components/Status.svelte';
	import { api } from '../lib/api';
	import { ago, datetime, label, pretty, short } from '../lib/format';
	import { href } from '../lib/router.svelte';
	import { ask, live, openLogs, toast } from '../lib/state.svelte';
	import type { Application, Comparison, Deployment, JobRun } from '../lib/types';

	let { name, tab }: { name: string; tab: string } = $props();

	let app = $state<Application | null>(null);
	let deployments = $state<Deployment[]>([]);
	let error = $state('');
	let comparison = $state<Comparison | null>(null);
	let comparing = $state('');
	let selectedJob = $state('');
	let runs = $state<JobRun[]>([]);
	let runsError = $state('');

	let timer: ReturnType<typeof setTimeout> | undefined;
	async function load() {
		clearTimeout(timer);
		try {
			const data = await api.application(name);
			app = data.application;
			deployments = data.deployments?.deployments ?? [];
			error = '';
		} catch (err) {
			error = (err as Error).message;
		}
		timer = setTimeout(load, document.hidden ? 10000 : 3000);
	}
	load();
	onDestroy(() => clearTimeout(timer));

	const current = $derived(deployments.find((deployment) => deployment.current));
	const allocations = $derived([...(app?.allocations ?? [])].sort((a, b) => a.service.localeCompare(b.service) || a.replica - b.replica));
	const services = $derived([...new Set(allocations.map((allocation) => allocation.service))]);
	const manifest = $derived(app ? pretty(app.canonicalManifestJson) : '');
	const tabs = $derived([
		{ id: 'overview', label: 'Overview' },
		{ id: 'replicas', label: 'Replicas', count: allocations.length },
		{ id: 'routes', label: 'Routes', count: (app?.routes?.length ?? 0) + (app?.tcpRoutes?.length ?? 0) },
		{ id: 'cron', label: 'Cron jobs', count: app?.cronJobs?.length ?? 0 },
		{ id: 'deployments', label: 'Deployments', count: deployments.length },
		{ id: 'manifest', label: 'Manifest' }
	]);

	function rollback(deployment: Deployment) {
		if (!app) return;
		const target = app;
		ask({
			title: `Roll back ${target.name} to generation ${deployment.generation}?`,
			body: `Revision ${short(deployment.revisionId)} is applied again as a new deployment.\nData volumes and secret values are not restored.`,
			action: 'Roll back',
			danger: true,
			success: `Rollback of ${target.name} started`,
			run: async () => {
				await api.rollback(target.name, deployment.revisionId, target.generation);
				load();
			}
		});
	}

	async function compare(deployment: Deployment) {
		if (!current) return;
		comparing = deployment.revisionId;
		try {
			comparison = await api.compare(name, deployment.revisionId, current.revisionId);
		} catch (err) {
			toast((err as Error).message, 'bad');
		} finally {
			comparing = '';
		}
	}

	async function showRuns(job: string) {
		selectedJob = job;
		runs = [];
		runsError = '';
		try {
			runs = (await api.cronRuns(name, job)).runs ?? [];
		} catch (err) {
			runsError = (err as Error).message;
		}
	}

	async function copy(text: string) {
		try {
			await navigator.clipboard.writeText(text);
			toast('Copied to clipboard', 'ok');
		} catch {
			toast('Clipboard is not available', 'bad');
		}
	}
</script>

<div class="page">
	<div class="crumbs"><a href="#/apps">Applications</a><ChevronRight size={14} /><span>{name}</span></div>
	<div class="page-head">
		<div>
			<div class="row">
				<h1>{name}</h1>
				{#if app}<Status value={app.executionStatus} pulse />{/if}
				{#if app?.deleted}<span class="chip">deleted</span>{/if}
			</div>
			{#if app}
				<p>
					Generation {app.activeGeneration} of {app.generation} active · revision <span class="mono">{short(app.revisionId)}</span> · updated {ago(app.updatedAt, live.now)}
				</p>
			{/if}
		</div>
		{#if services.length}
			<div class="row">
				{#each services.slice(0, 3) as service (service)}
					<button class="btn" onclick={() => openLogs({ kind: 'service', app: name, service, replica: allocations.find((a) => a.service === service)?.replica ?? 0 })}>
						<ScrollText size={14} /> {service} logs
					</button>
				{/each}
			</div>
		{/if}
	</div>

	{#if error && !app}
		<section class="panel"><Empty title="Could not load {name}">{error}</Empty></section>
	{:else if !app}
		<section class="panel"><Empty title="Loading…" /></section>
	{:else}
		{#if app.executionMessage}
			<div class="notice">
				<strong>{label(app.executionStatus)}</strong>
				{#if app.executionReason}<span class="chip mono">{app.executionReason}</span>{/if}
				<span>{app.executionMessage}</span>
			</div>
		{/if}

		<nav class="tabs">
			{#each tabs as item (item.id)}
				<a href={href('apps', name, item.id)} class:active={tab === item.id}>
					{item.label}
					{#if item.count !== undefined}<span class="count">{item.count}</span>{/if}
				</a>
			{/each}
		</nav>

		{#if tab === 'overview' || tab === 'replicas'}
			{#if tab === 'overview'}
				<div class="grid two">
					<section class="panel">
						<div class="panel-head"><h2>Details</h2></div>
						<dl class="kv panel-body">
							<dt>Status</dt><dd><Status value={app.executionStatus} /></dd>
							<dt>Desired generation</dt><dd class="mono">{app.generation}</dd>
							<dt>Active generation</dt><dd class="mono">{app.activeGeneration}</dd>
							<dt>Observed generation</dt><dd class="mono">{app.observedGeneration}</dd>
							<dt>Revision</dt><dd class="mono">{app.revisionId}</dd>
							<dt>Digest</dt><dd class="mono">{short(app.digest, 24)}</dd>
							<dt>Created</dt><dd>{datetime(app.createdAt)}</dd>
							<dt>Updated</dt><dd>{datetime(app.updatedAt)}</dd>
						</dl>
					</section>
					<section class="panel">
						<div class="panel-head"><h2>Endpoints</h2></div>
						{#if app.routes?.length || app.tcpRoutes?.length}
							<ul class="endpoints">
								{#each app.routes ?? [] as route (route.domain + route.nodeName)}
									<li>
										<a class="link" href="{route.https ? 'https' : 'http'}://{route.domain}" target="_blank" rel="noreferrer noopener">
											{route.https ? 'https' : 'http'}://{route.domain} <ExternalLink size={12} />
										</a>
										<span class="faint">→ {route.service}</span>
										<Status value={route.state} />
									</li>
								{/each}
								{#each app.tcpRoutes ?? [] as route (route.publicPort + route.nodeName)}
									<li>
										<span class="mono">tcp :{route.publicPort}</span>
										<span class="faint">→ {route.service}:{route.targetPort}</span>
										<Status value={route.state} />
									</li>
								{/each}
							</ul>
						{:else}
							<Empty title="No public routes" />
						{/if}
					</section>
				</div>
			{/if}

			<section class="panel spaced">
				<div class="panel-head"><h2>Replicas</h2><span class="hint">{allocations.length} placed</span></div>
				{#if allocations.length}
					<div class="table-wrap">
						<table class="data">
							<thead>
								<tr><th>Service</th><th>Replica</th><th>Phase</th><th>Health</th><th>Node</th><th>Address</th><th class="num">Restarts</th><th>Ready</th><th>Message</th><th></th></tr>
							</thead>
							<tbody>
								{#each allocations as allocation (allocation.id || allocation.service + allocation.replica)}
									<tr>
										<td class="primary-cell">{allocation.service}</td>
										<td class="mono">#{allocation.replica}</td>
										<td><Status value={allocation.phase} /></td>
										<td><Status value={allocation.healthStatus} /></td>
										<td>{#if allocation.nodeName}<a class="link" href={href('nodes', allocation.nodeName)}>{allocation.nodeName}</a>{:else}<span class="faint">unplaced</span>{/if}</td>
										<td class="mono faint">{allocation.address || '—'}</td>
										<td class="num" class:warn={allocation.restartCount > 0}>{allocation.restartCount}</td>
										<td class="faint">{ago(allocation.readyAt, live.now)}</td>
										<td class="wrap">{allocation.message || allocation.reason || allocation.placementReason}</td>
										<td>
											<button class="btn sm" onclick={() => openLogs({ kind: 'service', app: name, service: allocation.service, replica: allocation.replica })}>
												<ScrollText size={13} /> Logs
											</button>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{:else}
					<Empty title="No replicas placed" />
				{/if}
			</section>
		{:else if tab === 'routes'}
			<section class="panel">
				<div class="panel-head"><h2>HTTP routes</h2></div>
				{#if app.routes?.length}
					<div class="table-wrap">
						<table class="data">
							<thead><tr><th>Domain</th><th>Service</th><th>State</th><th>Certificate</th><th class="num">Upstreams</th><th>Node</th><th>Message</th></tr></thead>
							<tbody>
								{#each app.routes as route (route.domain + route.nodeName)}
									<tr>
										<td class="primary-cell">
											<a class="link" href="{route.https ? 'https' : 'http'}://{route.domain}" target="_blank" rel="noreferrer noopener">{route.domain}</a>
										</td>
										<td>{route.service}</td>
										<td><Status value={route.state} /></td>
										<td>{#if route.https}<Status value={route.certificateState} />{:else}<span class="faint">http only</span>{/if}</td>
										<td class="num">{route.upstreamCount}</td>
										<td>{route.nodeName}</td>
										<td class="wrap">{route.message || route.reason}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{:else}
					<Empty title="No HTTP routes" />
				{/if}
			</section>
			<section class="panel spaced">
				<div class="panel-head"><h2>TCP routes</h2></div>
				{#if app.tcpRoutes?.length}
					<div class="table-wrap">
						<table class="data">
							<thead><tr><th>Port</th><th>Service</th><th>State</th><th class="num">Upstreams</th><th>Node</th><th>Message</th></tr></thead>
							<tbody>
								{#each app.tcpRoutes as route (route.publicPort + route.nodeName)}
									<tr>
										<td class="mono">{route.publicPort} <ArrowRight size={12} /> {route.targetPort}</td>
										<td>{route.service}</td>
										<td><Status value={route.state} /></td>
										<td class="num">{route.upstreamCount}</td>
										<td>{route.nodeName}</td>
										<td class="wrap">{route.message}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{:else}
					<Empty title="No TCP routes" />
				{/if}
			</section>
		{:else if tab === 'cron'}
			<div class="grid two">
				<section class="panel">
					<div class="panel-head"><h2>Schedules</h2></div>
					{#if app.cronJobs?.length}
						<div class="table-wrap">
							<table class="data">
								<thead><tr><th>Job</th><th>Last run</th><th>Next run</th></tr></thead>
								<tbody>
									{#each app.cronJobs as job (job.job)}
										<tr class="clickable" class:selected={selectedJob === job.job} onclick={() => showRuns(job.job)}>
											<td class="primary-cell">{job.job}</td>
											<td><Status value={job.lastState} text={job.lastState === 'JOB_RUN_STATE_UNSPECIFIED' || !job.lastState ? 'never' : ''} /></td>
											<td class="faint" title={datetime(job.nextRunAt)}>{ago(job.nextRunAt, live.now)}</td>
										</tr>
									{/each}
								</tbody>
							</table>
						</div>
					{:else}
						<Empty title="No cron jobs" />
					{/if}
				</section>
				<section class="panel">
					<div class="panel-head"><h2>{selectedJob ? `Runs of ${selectedJob}` : 'Runs'}</h2></div>
					{#if !selectedJob}
						<Empty title="Select a job to see its runs" />
					{:else if runsError}
						<Empty title="Could not load runs">{runsError}</Empty>
					{:else if runs.length}
						<div class="table-wrap">
							<table class="data">
								<thead><tr><th>State</th><th>Scheduled</th><th class="num">Exit</th><th></th></tr></thead>
								<tbody>
									{#each runs as run (run.id)}
										<tr>
											<td><Status value={run.state} /></td>
											<td class="faint" title={datetime(run.scheduledAt)}>{ago(run.scheduledAt, live.now)}</td>
											<td class="num mono">{run.hasExitCode ? run.exitCode : '—'}</td>
											<td>
												<button class="btn sm" onclick={() => openLogs({ kind: 'run', app: name, job: run.job || selectedJob, run: run.id, label: `${selectedJob} · ${ago(run.scheduledAt, live.now)}` })}>
													<ScrollText size={13} /> Logs
												</button>
											</td>
										</tr>
									{/each}
								</tbody>
							</table>
						</div>
					{:else}
						<Empty title="No retained runs" />
					{/if}
				</section>
			</div>
		{:else if tab === 'deployments'}
			<section class="panel">
				<div class="panel-head"><h2>History</h2><span class="hint">Newest first</span></div>
				{#if deployments.length}
					<ol class="history">
						{#each deployments as deployment (deployment.revisionId)}
							<li class:current={deployment.current}>
								<div class="marker"><History size={14} /></div>
								<div class="body">
									<div class="row">
										<span class="gen">Generation {deployment.generation}</span>
										<Status value={deployment.outcome || deployment.executionStatus} />
										{#if deployment.current}<span class="chip accent">current</span>{/if}
										{#if deployment.active}<span class="chip">active</span>{/if}
										{#if deployment.rollbackFromRevisionId}<span class="chip">rollback of {short(deployment.rollbackFromRevisionId, 8)}</span>{/if}
									</div>
									<div class="faint meta">
										<span class="mono">{short(deployment.revisionId)}</span> · {datetime(deployment.createdAt)} ({ago(deployment.createdAt, live.now)})
									</div>
									{#if deployment.source}
										{@const source = deployment.source}
										<div class="faint meta">
											From <span class="mono">{source.repository}{source.commit ? '@' + short(source.commit, 7) : ''}</span>{source.ref ? ` (${source.ref})` : ''}{source.actor ? ` by ${source.actor}` : ''}
											{#if source.runUrl}· <a class="link" href={source.runUrl} target="_blank" rel="noreferrer noopener">run <ExternalLink size={12} /></a>{/if}
										</div>
									{/if}
									{#if deployment.message}<div class="muted">{deployment.message}</div>{/if}
									{#if deployment.images?.length}
										<div class="images">
											{#each deployment.images as image (image.kind + image.name)}
												<span class="chip mono" title={image.requestedImage}>{image.name}: {image.image || image.requestedImage}</span>
											{/each}
										</div>
									{/if}
								</div>
								{#if !deployment.current}
									<div class="row actions">
										<button class="btn sm" disabled={!current || comparing === deployment.revisionId} onclick={() => compare(deployment)}>
											<GitCompare size={13} /> Diff vs current
										</button>
										<button class="btn sm" onclick={() => rollback(deployment)}><RotateCcw size={13} /> Roll back</button>
									</div>
								{/if}
							</li>
						{/each}
					</ol>
				{:else}
					<Empty title="No deployment history" />
				{/if}
			</section>

			{#if comparison}
				<section class="panel spaced">
					<div class="panel-head">
						<h2>Generation {comparison.from?.generation} → {comparison.to?.generation}</h2>
						<button class="btn sm ghost" onclick={() => (comparison = null)}>Close</button>
					</div>
					{#if comparison.changes?.length}
						<div class="table-wrap">
							<table class="data diff">
								<thead><tr><th>Change</th><th>Path</th><th>Before</th><th>After</th></tr></thead>
								<tbody>
									{#each comparison.changes as change, index (index)}
										<tr>
											<td><span class="chip">{change.kind}</span></td>
											<td class="mono">{change.path}</td>
											<td class="mono wrap removed">{change.redacted ? '•••' : change.before}</td>
											<td class="mono wrap added">{change.redacted ? '•••' : change.after}</td>
										</tr>
									{/each}
								</tbody>
							</table>
						</div>
						{#if comparison.truncated}<p class="faint pad">The diff was truncated.</p>{/if}
					{:else}
						<Empty title="No differences" />
					{/if}
				</section>
			{/if}
		{:else if tab === 'manifest'}
			<section class="panel">
				<div class="panel-head">
					<h2>Canonical manifest</h2>
					<button class="btn sm" onclick={() => copy(manifest)}><Copy size={13} /> Copy</button>
				</div>
				<div class="panel-body"><pre class="code">{manifest}</pre></div>
			</section>
		{/if}
	{/if}
</div>

<style>
	.two {
		grid-template-columns: repeat(auto-fit, minmax(380px, 1fr));
		align-items: start;
	}
	.spaced {
		margin-top: 16px;
	}
	.notice {
		display: flex;
		gap: 10px;
		align-items: center;
		flex-wrap: wrap;
		padding: 12px 16px;
		margin-bottom: 20px;
		border-radius: var(--radius);
		background: var(--warn-soft);
		color: var(--text);
		border: 1px solid color-mix(in srgb, var(--warn) 25%, transparent);
	}
	.notice strong {
		text-transform: capitalize;
		color: var(--warn);
	}
	.endpoints {
		list-style: none;
		margin: 0;
		padding: 8px 16px;
	}
	.endpoints li {
		display: flex;
		align-items: center;
		gap: 10px;
		padding: 8px 0;
		flex-wrap: wrap;
	}
	.endpoints .link {
		display: inline-flex;
		align-items: center;
		gap: 4px;
		font-weight: 500;
	}
	.warn {
		color: var(--warn);
		font-weight: 600;
	}
	.history {
		list-style: none;
		margin: 0;
		padding: 8px 0;
	}
	.history li {
		display: flex;
		gap: 14px;
		padding: 14px 16px;
		align-items: flex-start;
	}
	.history li + li {
		border-top: 1px solid var(--border);
	}
	.history li.current {
		background: color-mix(in srgb, var(--accent-soft) 45%, transparent);
	}
	.marker {
		width: 28px;
		height: 28px;
		border-radius: 50%;
		display: grid;
		place-items: center;
		background: var(--surface-2);
		color: var(--text-3);
		flex: none;
	}
	.current .marker {
		background: var(--accent);
		color: #fff;
	}
	.body {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 4px;
	}
	.gen {
		font-weight: 600;
	}
	.meta {
		font-size: 12.5px;
	}
	.images {
		display: flex;
		flex-wrap: wrap;
		gap: 6px;
		margin-top: 4px;
	}
	.images .chip {
		max-width: 100%;
		overflow: hidden;
		text-overflow: ellipsis;
		font-size: 11.5px;
	}
	.accent {
		background: var(--accent-soft);
		color: var(--accent-text);
		border-color: transparent;
	}
	.actions {
		flex: none;
	}
	.diff .removed {
		color: var(--bad);
		background: color-mix(in srgb, var(--bad-soft) 50%, transparent);
	}
	.diff .added {
		color: var(--ok);
		background: color-mix(in srgb, var(--ok-soft) 50%, transparent);
	}
	.pad {
		padding: 0 16px 12px;
	}
</style>
