<script lang="ts">
	import { AppWindow, Boxes, Cpu, MemoryStick, Server } from '@lucide/svelte';
	import Meter from '../components/Meter.svelte';
	import Stat from '../components/Stat.svelte';
	import Status from '../components/Status.svelte';
	import { ago, bytes, cores, label, ratio, tone } from '../lib/format';
	import { href } from '../lib/router.svelte';
	import { live, openLogs } from '../lib/state.svelte';
	import type { Runtime } from '../lib/types';

	const snapshot = $derived(live.snapshot!);
	const cluster = $derived(snapshot.cluster);
	const apps = $derived(snapshot.applications ?? []);
	const nodes = $derived(snapshot.nodes ?? []);
	const runtimes = $derived(snapshot.runtimes?.runtimes ?? []);
	const backup = $derived(snapshot.backup);
	const backupStale = $derived(!!backup?.enabled && !!backup.latestAt && live.now - new Date(backup.latestAt).getTime() > 48 * 3600 * 1000);
	const events = $derived([...(snapshot.events?.events ?? [])].reverse().slice(0, 12));

	// A settled application reports HEALTHY rather than RUNNING, so count every
	// status in the ok tone.
	const runningApps = $derived(apps.filter((app) => tone(app.executionStatus) === 'ok').length);
	const connected = $derived(nodes.filter((node) => label(node.connectionStatus) === 'connected').length);
	const services = $derived(runtimes.filter((runtime) => label(runtime.kind) === 'service'));
	const healthy = $derived(services.filter((runtime) => tone(runtime.healthStatus) === 'ok' || label(runtime.allocationPhase) === 'running').length);

	// Only the newest run of each cron job counts: a later run supersedes a
	// failure, and a run stopped because its job was removed, replaced or
	// cancelled is not something to fix. One-off runs (run-…) live on the Jobs
	// page, and a failed pre-deploy job already fails its application.
	const time = (value: string | null) => (value ? new Date(value).getTime() : 0);
	const latestRuns = $derived.by(() => {
		const newest = new Map<string, Runtime>();
		for (const runtime of runtimes) {
			if (label(runtime.kind) !== 'cron run') continue;
			const key = `${runtime.application}\x00${runtime.resource}`;
			const seen = newest.get(key);
			if (!seen || time(runtime.scheduledAt) > time(seen.scheduledAt)) newest.set(key, runtime);
		}
		return new Set([...newest.values()].map((runtime) => runtime.id));
	});
	const stoppedReasons = ['job_removed', 'job_replaced', 'cancelled'];
	const oneOff = (runtime: Runtime) => runtime.resource === `run-${runtime.id.replace(/^job_/, '')}`;
	const failedJob = (runtime: Runtime) =>
		tone(runtime.jobRunState) === 'bad' && latestRuns.has(runtime.id) && !oneOff(runtime) && !stoppedReasons.includes(runtime.reason);

	const sum = (key: 'assignedCpuMillis' | 'allocatableCpuMillis' | 'assignedMemoryBytes' | 'allocatableMemoryBytes') =>
		nodes.reduce((total, node) => total + Number(node[key] || 0), 0);
	const cpuUsed = $derived(sum('assignedCpuMillis'));
	const cpuTotal = $derived(sum('allocatableCpuMillis'));
	const memUsed = $derived(sum('assignedMemoryBytes'));
	const memTotal = $derived(sum('allocatableMemoryBytes'));

	const problems = $derived([
		...(backup?.enabled && (backup.failure || backupStale)
			? [{ key: 'backup', href: '#/', title: 'Controller backups', kind: 'Backup', status: backup.failure ? 'failed' : 'degraded', detail: backup.failure || `Latest backup ${ago(backup.latestAt, live.now)}` }]
			: []),
		...apps
			.filter((app) => ['bad', 'warn'].includes(tone(app.executionStatus)))
			.map((app) => ({ key: `a${app.name}`, href: href('apps', app.name), title: app.name, kind: 'Application', status: app.executionStatus, detail: app.executionMessage || app.executionReason })),
		...nodes
			.filter((node) => tone(node.connectionStatus) === 'bad' || node.drainBlockers?.length)
			.map((node) => ({ key: `n${node.name}`, href: href('nodes', node.name), title: node.name, kind: 'Node', status: node.connectionStatus, detail: node.drainBlockers?.join(', ') || `Last seen ${ago(node.lastSeenAt, live.now)}` })),
		...runtimes
			.filter((runtime) => (label(runtime.kind) === 'cron run' ? failedJob(runtime) : tone(runtime.healthStatus) === 'bad' || tone(runtime.allocationPhase) === 'bad'))
			.slice(0, 8)
			.map((runtime) => ({ key: `r${runtime.id}`, href: href('apps', runtime.application), title: `${runtime.application} / ${runtime.resource}`, kind: 'Runtime', status: runtime.jobRunState !== 'JOB_RUN_STATE_UNSPECIFIED' && runtime.jobRunState ? runtime.jobRunState : runtime.healthStatus, detail: runtime.message || runtime.reason }))
	]);
</script>

<div class="page">
	<div class="page-head">
		<div>
			<h1>{cluster?.name || 'Cluster'}</h1>
			<p>
				{cluster?.endpoint || 'Endpoint unavailable'}
				{#if cluster?.workloadCidr}<span class="faint"> · workloads {cluster.workloadCidr}</span>{/if}
				{#if backup}
					<span class="faint" title={backup.latestPath}>
						· {!backup.enabled ? 'automatic backups off' : backup.latestAt ? `backed up ${ago(backup.latestAt, live.now)}` : 'no backup yet'}
					</span>
				{/if}
			</p>
		</div>
		<span class="chip">Generation {cluster?.generation ?? '0'}</span>
	</div>

	<div class="grid stats">
		<Stat label="Applications" value="{runningApps}/{apps.length}" detail="running" icon={AppWindow} href="#/apps" />
		<Stat label="Nodes" value="{connected}/{nodes.length}" detail="connected" icon={Server} href="#/nodes" />
		<Stat label="Replicas" value="{healthy}/{services.length}" detail="healthy" icon={Boxes} href="#/runtimes" />
		<Stat label="CPU reserved" value="{Math.round(ratio(cpuUsed, cpuTotal) * 100)}%" detail="{cores(cpuUsed)} of {cores(cpuTotal)} cores" icon={Cpu} />
		<Stat label="Memory reserved" value="{Math.round(ratio(memUsed, memTotal) * 100)}%" detail="{bytes(memUsed)} of {bytes(memTotal)}" icon={MemoryStick} />
	</div>

	<div class="grid columns">
		<div class="stack">
			<section class="panel">
				<div class="panel-head">
					<h2>Needs attention</h2>
					<span class="hint">{problems.length ? `${problems.length} issues` : 'All clear'}</span>
				</div>
				{#if problems.length}
					<ul class="list">
						{#each problems as problem (problem.key)}
							<li>
								<a href={problem.href}>
									<div class="main">
										<div class="title">{problem.title} <span class="faint">· {problem.kind}</span></div>
										{#if problem.detail}<div class="detail">{problem.detail}</div>{/if}
									</div>
									<Status value={problem.status} />
								</a>
							</li>
						{/each}
					</ul>
				{:else}
					<div class="allclear">
						<div class="ring"><span></span></div>
						<div>
							<div class="title">Everything is running</div>
							<div class="faint">No failing applications, nodes, or replicas.</div>
						</div>
					</div>
				{/if}
			</section>

			<section class="panel">
				<div class="panel-head">
					<h2>Applications</h2>
					<a class="link" href="#/apps">View all</a>
				</div>
				<div class="table-wrap">
					<table class="data">
						<thead><tr><th>Name</th><th>Status</th><th>Generation</th><th>Updated</th></tr></thead>
						<tbody>
							{#each apps.slice(0, 8) as app (app.name)}
								<tr class="clickable" onclick={() => (location.hash = href('apps', app.name))}>
									<td class="primary-cell"><a href={href('apps', app.name)}>{app.name}</a></td>
									<td><Status value={app.executionStatus} pulse /></td>
									<td class="mono">{app.activeGeneration}<span class="faint">/{app.generation}</span></td>
									<td class="faint">{ago(app.updatedAt, live.now)}</td>
								</tr>
							{:else}
								<tr><td colspan="4" class="empty">No applications deployed yet. Run <code>orch deploy</code>.</td></tr>
							{/each}
						</tbody>
					</table>
				</div>
			</section>
		</div>

		<div class="stack">
			<section class="panel">
				<div class="panel-head">
					<h2>Nodes</h2>
					<a class="link" href="#/nodes">View all</a>
				</div>
				<ul class="list">
					{#each nodes as node (node.name)}
						<li>
							<a href={href('nodes', node.name)} class="node">
								<div class="row between">
									<span class="title">{node.name}</span>
									<Status value={node.connectionStatus} />
								</div>
								<div class="meters">
									<Meter value={ratio(node.assignedCpuMillis, node.allocatableCpuMillis)} label="CPU" detail="{cores(node.assignedCpuMillis)}/{cores(node.allocatableCpuMillis)}" />
									<Meter value={ratio(node.assignedMemoryBytes, node.allocatableMemoryBytes)} label="Memory" detail="{bytes(node.assignedMemoryBytes)}/{bytes(node.allocatableMemoryBytes)}" />
								</div>
							</a>
						</li>
					{:else}
						<li class="empty">No nodes have joined.</li>
					{/each}
				</ul>
			</section>

			<section class="panel">
				<div class="panel-head">
					<h2>Activity</h2>
					<a class="link" href="#/events">All events</a>
				</div>
				<ol class="timeline">
					{#each events as event (event.sequence)}
						<li>
							<span class="bullet"></span>
							<div>
								<div class="kind mono">{event.kind}</div>
								<div class="faint small">{ago(event.createdAt, live.now)} · #{event.sequence}</div>
							</div>
						</li>
					{:else}
						<li class="empty">No events yet.</li>
					{/each}
				</ol>
			</section>

			{#if services.length}
				<section class="panel">
					<div class="panel-head"><h2>Quick logs</h2></div>
					<div class="quick">
						{#each services.slice(0, 6) as runtime (runtime.id)}
							<button class="chip" onclick={() => openLogs({ kind: 'service', app: runtime.application, service: runtime.resource, replica: runtime.replica })}>
								{runtime.application}/{runtime.resource} #{runtime.replica}
							</button>
						{/each}
					</div>
				</section>
			{/if}
		</div>
	</div>
</div>

<style>
	.stats {
		grid-template-columns: repeat(auto-fit, minmax(min(170px, 45%), 1fr));
		margin-bottom: 16px;
	}
	.columns {
		grid-template-columns: minmax(0, 1.6fr) minmax(0, 1fr);
		align-items: start;
	}
	@media (max-width: 1100px) {
		.columns {
			grid-template-columns: 1fr;
		}
	}
	.list {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.list li + li {
		border-top: 1px solid var(--border);
	}
	.list a {
		display: flex;
		align-items: center;
		gap: 12px;
		padding: 12px 16px;
	}
	.list a:hover {
		background: var(--surface-2);
	}
	.list a.node {
		flex-direction: column;
		align-items: stretch;
	}
	.between {
		justify-content: space-between;
	}
	.main {
		flex: 1;
		min-width: 0;
	}
	.title {
		font-weight: 550;
	}
	.detail {
		color: var(--text-2);
		font-size: 13px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.meters {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 16px;
	}
	.allclear {
		display: flex;
		align-items: center;
		gap: 14px;
		padding: 20px 16px;
	}
	.ring {
		width: 36px;
		height: 36px;
		border-radius: 50%;
		background: var(--ok-soft);
		display: grid;
		place-items: center;
	}
	.ring span {
		width: 12px;
		height: 12px;
		border-radius: 50%;
		background: var(--ok);
	}
	.timeline {
		list-style: none;
		margin: 0;
		padding: 12px 16px;
	}
	.timeline li {
		display: flex;
		gap: 12px;
		padding: 6px 0;
		position: relative;
	}
	.timeline li:not(:last-child)::before {
		content: '';
		position: absolute;
		left: 3.5px;
		top: 18px;
		bottom: -8px;
		width: 1px;
		background: var(--border);
	}
	.bullet {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: var(--accent);
		margin-top: 6px;
		flex: none;
	}
	.kind {
		font-size: 12.5px;
	}
	.small {
		font-size: 12px;
	}
	.quick {
		display: flex;
		flex-wrap: wrap;
		gap: 6px;
		padding: 14px 16px;
	}
	.quick .chip {
		cursor: pointer;
		font-family: var(--mono);
	}
	.quick .chip:hover {
		border-color: var(--accent);
		color: var(--accent-text);
	}
</style>
