<script lang="ts">
	import { Boxes, ChevronRight, Copy, Cpu, ExternalLink, MemoryStick, RotateCcw, ScrollText } from '@lucide/svelte';
	import { onDestroy } from 'svelte';
	import DatabaseBackups from '../components/DatabaseBackups.svelte';
	import Empty from '../components/Empty.svelte';
	import Meter from '../components/Meter.svelte';
	import Stat from '../components/Stat.svelte';
	import Status from '../components/Status.svelte';
	import { api } from '../lib/api';
	import { ago, bytes, cores, datetime, detail, ratio, short, tone } from '../lib/format';
	import { href } from '../lib/router.svelte';
	import { live, openLogs, toast } from '../lib/state.svelte';
	import type { Application, Deployment } from '../lib/types';

	let { app: appName, service }: { app: string; service: string } = $props();

	let app = $state<Application | null>(null);
	let deployments = $state<Deployment[]>([]);
	let error = $state('');

	let timer: ReturnType<typeof setTimeout> | undefined;
	async function load() {
		clearTimeout(timer);
		try {
			const data = await api.application(appName);
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

	// The service's slice of the canonical manifest. Databases are services
	// orch generates from the manifest's `databases` block.
	type ServiceSpec = {
		image?: string;
		replicas?: number;
		restart?: string;
		resources?: { cpu?: string; memory?: string };
		http?: { port: number; public?: boolean; domains?: string[]; health_path?: string };
		tcp?: { port: number; public_port?: number; node_port?: number };
		udp?: { port: number; node_port: number };
		depends_on?: string[];
		mounts?: { source: string; target: string; read_only: boolean }[];
		autoscale?: { min: number; max: number; cpu_percent: number };
		database?: { engine: string; version: string };
	};
	type ManifestJson = {
		services?: Record<string, ServiceSpec>;
		volumes?: Record<string, { backup?: { schedule: string; keep: number } }>;
	};
	const manifest = $derived.by((): ManifestJson => {
		try {
			return JSON.parse(app?.canonicalManifestJson || '{}');
		} catch {
			return {};
		}
	});
	const spec = $derived(manifest.services?.[service]);
	const database = $derived(spec?.database);
	const dataVolume = $derived(manifest.volumes?.[`${service}-data`] ? `${service}-data` : '');
	const backup = $derived(manifest.volumes?.[`${service}-backup`]?.backup ?? manifest.volumes?.[`${service}-data`]?.backup);
	const host = $derived(`${service}.${appName}.svc.internal`);
	const port = $derived(spec?.http?.port ?? spec?.tcp?.port ?? spec?.udp?.port);

	const replicas = $derived((app?.allocations ?? []).filter((allocation) => allocation.service === service).sort((a, b) => a.replica - b.replica));
	// Requests come from the runtime inventory, which carries them as numbers.
	const runtimes = $derived(new Map((live.snapshot?.runtimes?.runtimes ?? []).map((runtime) => [runtime.id, runtime])));
	const rows = $derived(
		replicas.map((allocation) => {
			const runtime = runtimes.get(allocation.id);
			const cpu = app?.allocationCpuMillis?.[allocation.id];
			const memory = app?.allocationMemoryBytes?.[allocation.id];
			return {
				allocation,
				cpu,
				memory,
				cpuRequest: Number(runtime?.cpuMillis || 0),
				memoryRequest: Number(runtime?.memoryBytes || 0)
			};
		})
	);
	const ready = $derived(replicas.filter((allocation) => tone(allocation.healthStatus) === 'ok').length);
	const restarts = $derived(replicas.reduce((total, allocation) => total + allocation.restartCount, 0));
	const reporting = $derived(rows.filter((row) => row.cpu !== undefined));
	const cpuUsed = $derived(reporting.reduce((total, row) => total + Number(row.cpu || 0), 0));
	// Agents older than memory reporting send CPU only.
	const memoryReporting = $derived(rows.filter((row) => row.memory !== undefined));
	const memoryUsed = $derived(memoryReporting.reduce((total, row) => total + Number(row.memory || 0), 0));
	const memoryRequestedReporting = $derived(memoryReporting.reduce((total, row) => total + row.memoryRequest, 0));
	const cpuRequested = $derived(rows.reduce((total, row) => total + row.cpuRequest, 0));
	const memoryRequested = $derived(rows.reduce((total, row) => total + row.memoryRequest, 0));
	const scale = $derived(app?.autoscale?.find((item) => item.service === service));
	// The worst replica decides the service's status.
	const status = $derived.by(() => {
		if (!replicas.length) return 'pending';
		const worst = replicas.find((allocation) => tone(allocation.phase) === 'bad' || tone(allocation.healthStatus) === 'bad') ?? replicas.find((allocation) => tone(allocation.healthStatus) !== 'ok');
		return worst ? (tone(worst.phase) === 'bad' || tone(worst.phase) === 'warn' ? worst.phase : worst.healthStatus) : replicas[0].healthStatus;
	});

	const routes = $derived((app?.routes ?? []).filter((route) => route.service === service));
	const tcpRoutes = $derived((app?.tcpRoutes ?? []).filter((route) => route.service === service));
	const nodePorts = $derived((app?.nodePorts ?? []).filter((nodePort) => nodePort.service === service));

	// Each deployment with the image this service ran, newest first, marking
	// the ones that changed it.
	const history = $derived(
		deployments.map((deployment, index) => {
			const image = (deployment: Deployment | undefined) => {
				const found = deployment?.images?.find((item) => item.kind === 'service' && item.name === service);
				return found ? found.image || found.requestedImage : '';
			};
			const current = image(deployment);
			const older = deployments[index + 1];
			return { deployment, image: current, changed: !!older && image(older) !== current };
		})
	);
	const currentImage = $derived(history.find((item) => item.deployment.current)?.image || spec?.image || '');

	function logs(replica = replicas[0]?.replica ?? 0) {
		openLogs({ kind: 'service', app: appName, service, replica });
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
	<div class="crumbs">
		<a href="#/apps">Applications</a><ChevronRight size={14} /><a href={href('apps', appName)}>{appName}</a><ChevronRight size={14} /><span>{service}</span>
	</div>
	<div class="page-head">
		<div>
			<div class="row">
				<h1>{service}</h1>
				{#if app && spec}<Status value={status} pulse />{/if}
				{#if database}<span class="chip">{database.engine} {database.version}</span>{/if}
			</div>
			{#if spec}
				<p>
					{database ? 'Database' : 'Service'} of <a class="link" href={href('apps', appName)}>{appName}</a>
					{#if currentImage}· <span class="mono">{currentImage}</span>{/if}
				</p>
			{/if}
		</div>
		{#if replicas.length}
			<div class="row"><button class="btn" onclick={() => logs()}><ScrollText size={14} /> Logs</button></div>
		{/if}
	</div>

	{#if error && !app}
		<section class="panel"><Empty title="Could not load {appName}">{error}</Empty></section>
	{:else if !app}
		<section class="panel"><Empty title="Loading…" /></section>
	{:else if !spec}
		<section class="panel">
			<Empty title="{appName} has no service {service}">It may have been removed in a later deployment. <a class="link" href={href('apps', appName)}>Back to {appName}</a></Empty>
		</section>
	{:else}
		<div class="grid stats">
			<Stat label="Replicas" value="{ready}/{replicas.length}" detail={scale ? `ready · autoscale ${scale.min}–${scale.max}` : 'ready'} icon={Boxes} />
			<Stat
				label="CPU in use"
				value={reporting.length ? `${cores(cpuUsed)} cores` : '—'}
				detail={reporting.length ? `${Math.round(ratio(cpuUsed, cpuRequested) * 100)}% of ${cores(cpuRequested)} requested` : 'waiting for the first report'}
				icon={Cpu}
			/>
			<Stat
				label="Memory in use"
				value={memoryReporting.length ? bytes(memoryUsed) : '—'}
				detail={memoryReporting.length ? `${Math.round(ratio(memoryUsed, memoryRequestedReporting) * 100)}% of ${bytes(memoryRequestedReporting)} requested` : `waiting for a report · ${bytes(memoryRequested)} requested`}
				icon={MemoryStick}
			/>
			<Stat label="Restarts" value={restarts} detail="across all replicas" icon={RotateCcw} />
		</div>

		<section class="panel spaced">
			<div class="panel-head"><h2>Replicas</h2><span class="hint">CPU averaged over the last minute, against what each replica requested</span></div>
			{#if rows.length}
				<div class="table-wrap">
					<table class="data">
						<thead>
							<tr><th>Replica</th><th>Status</th><th>Node</th><th>CPU</th><th>Memory</th><th class="num">Restarts</th><th>Ready</th><th></th></tr>
						</thead>
						<tbody>
							{#each rows as row (row.allocation.id || row.allocation.replica)}
								{@const allocation = row.allocation}
								{@const note = detail(allocation.message || allocation.reason || allocation.placementReason, allocation.phase, allocation.healthStatus)}
								<tr>
									<td class="primary-cell mono">#{allocation.replica}</td>
									<td>
										<div class="row tight"><Status value={allocation.phase} /><Status value={allocation.healthStatus} /></div>
										{#if note}<div class="note">{note}</div>{/if}
									</td>
									<td>{#if allocation.nodeName}<a class="link" href={href('nodes', allocation.nodeName)}>{allocation.nodeName}</a>{:else}<span class="faint">unplaced</span>{/if}</td>
									<td class="usage">
										{#if row.cpu !== undefined}
											<Meter value={ratio(row.cpu, row.cpuRequest)} label={cores(row.cpu)} detail="of {cores(row.cpuRequest)}" />
										{:else}<span class="faint">—</span>{/if}
									</td>
									<td class="usage">
										{#if row.memory !== undefined}
											<Meter value={ratio(row.memory, row.memoryRequest)} label={bytes(row.memory)} detail="of {bytes(row.memoryRequest)}" />
										{:else}<span class="faint">—</span>{/if}
									</td>
									<td class="num" class:warn={allocation.restartCount > 0}>{allocation.restartCount}</td>
									<td class="faint">{ago(allocation.readyAt, live.now)}</td>
									<td><button class="btn sm" onclick={() => logs(allocation.replica)}><ScrollText size={13} /> Logs</button></td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{:else}
				<Empty title="No replicas placed" />
			{/if}
		</section>

		<div class="grid two spaced">
			<section class="panel">
				<div class="panel-head"><h2>Details</h2></div>
				<dl class="kv panel-body">
					<dt>Image</dt><dd class="mono wrap">{currentImage || '—'}</dd>
					<dt>Requests</dt><dd>{spec.resources?.cpu || '—'} CPU · {spec.resources?.memory || '—'} memory per replica</dd>
					<dt>Replicas</dt><dd>{scale ? `${scale.replicas} (autoscale ${scale.min}–${scale.max} at ${scale.cpuTargetPercent}% CPU)` : (spec.replicas ?? replicas.length)}</dd>
					<dt>Internal address</dt>
					<dd class="mono"><button class="copy" title="Copy address" onclick={() => copy(port ? `${host}:${port}` : host)}>{host}{port ? `:${port}` : ''} <Copy size={12} /></button></dd>
					{#if spec.restart}<dt>Restart</dt><dd>{spec.restart}</dd>{/if}
					{#if spec.http?.health_path}<dt>Health check</dt><dd class="mono">{spec.http.health_path}</dd>{/if}
					{#if spec.depends_on?.length}<dt>Depends on</dt><dd>{#each spec.depends_on as dependency, index (dependency)}{index ? ', ' : ''}<a class="link" href={href('apps', appName, 'services', dependency)}>{dependency}</a>{/each}</dd>{/if}
					{#if spec.mounts?.length}<dt>Mounts</dt><dd class="mono">{#each spec.mounts as mount (mount.target)}<div>{mount.source} → {mount.target}{mount.read_only ? ' (ro)' : ''}</div>{/each}</dd>{/if}
				</dl>
			</section>

			{#if database}
				<section class="panel">
					<div class="panel-head"><h2>Database</h2><span class="hint">Reach it with connect in orch.pkl</span></div>
					<dl class="kv panel-body">
						<dt>Engine</dt><dd>{database.engine} {database.version}</dd>
						<dt>URL secret</dt><dd class="mono">{service}-url</dd>
						<dt>Volume</dt><dd class="mono">{dataVolume || 'in memory'}</dd>
						<dt>Backups</dt><dd>{#if backup}<span class="mono">{backup.schedule}</span> <span class="faint">· keep {backup.keep}</span>{:else}<span class="faint">off</span>{/if}</dd>
					</dl>
				</section>
			{:else}
				<section class="panel">
					<div class="panel-head"><h2>Endpoints</h2></div>
					{@render endpoints()}
				</section>
			{/if}
		</div>

		{#if database}
			{#if routes.length || tcpRoutes.length || nodePorts.length}
				<section class="panel spaced">
					<div class="panel-head"><h2>Endpoints</h2></div>
					{@render endpoints()}
				</section>
			{/if}
			<DatabaseBackups app={appName} database={service} />
		{/if}

		<section class="panel spaced">
			<div class="panel-head">
				<h2>Deployments</h2>
				<a class="link" href={href('apps', appName, 'deployments')}>Diff and roll back</a>
			</div>
			{#if history.length}
				<div class="table-wrap">
					<table class="data">
						<thead><tr><th>Generation</th><th>Outcome</th><th>Image</th><th>Revision</th><th>Deployed</th></tr></thead>
						<tbody>
							{#each history as item (item.deployment.revisionId)}
								<tr class:current={item.deployment.current}>
									<td class="mono">
										{item.deployment.generation}
										{#if item.deployment.current}<span class="chip accent">current</span>{/if}
									</td>
									<td><Status value={item.deployment.outcome || item.deployment.executionStatus} /></td>
									<td class="mono wrap" class:faint={!item.changed}>{item.image || '—'}{#if item.changed && item.image}<span class="chip">changed</span>{/if}</td>
									<td class="mono faint">{short(item.deployment.revisionId)}</td>
									<td class="faint" title={datetime(item.deployment.createdAt)}>{ago(item.deployment.createdAt, live.now)}</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{:else}
				<Empty title="No deployment history" />
			{/if}
		</section>
	{/if}
</div>

{#snippet endpoints()}
	{#if routes.length || tcpRoutes.length || nodePorts.length}
		<ul class="endpoints">
			{#each routes as route (route.domain + route.nodeName)}
				<li>
					<a class="link" href="{route.https ? 'https' : 'http'}://{route.domain}" target="_blank" rel="noreferrer noopener">
						{route.https ? 'https' : 'http'}://{route.domain} <ExternalLink size={12} />
					</a>
					<Status value={route.state} />
					{#if detail(route.message || route.reason, route.state, route.https ? route.certificateState : '')}<span class="note">{route.message || route.reason}</span>{/if}
				</li>
			{/each}
			{#each tcpRoutes as route (route.publicPort + route.nodeName)}
				<li>
					<span class="mono">tcp :{route.publicPort}</span>
					<span class="faint">→ {route.targetPort}</span>
					<Status value={route.state} />
				</li>
			{/each}
			{#each nodePorts as nodePort (nodePort.allocationId + nodePort.protocol + nodePort.nodePort)}
				<li>
					<span class="mono">{nodePort.protocol} {nodePort.nodeName}:{nodePort.nodePort}</span>
					<span class="faint">→ {nodePort.targetPort}</span>
					<Status value={nodePort.state} />
				</li>
			{/each}
		</ul>
	{:else}
		<Empty title="No public routes" />
	{/if}
{/snippet}

<style>
	.stats {
		grid-template-columns: repeat(auto-fit, minmax(190px, 1fr));
	}
	.two {
		grid-template-columns: repeat(auto-fit, minmax(380px, 1fr));
		align-items: start;
	}
	.spaced {
		margin-top: 16px;
	}
	.tight {
		gap: 6px;
	}
	.note {
		margin-top: 4px;
		font-size: 12.5px;
		color: var(--text-2);
		white-space: normal;
	}
	.usage {
		min-width: 150px;
	}
	.warn {
		color: var(--warn);
		font-weight: 600;
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
	td .chip {
		margin-left: 6px;
	}
	.accent {
		background: var(--accent-soft);
		color: var(--accent-text);
		border-color: transparent;
	}
	tr.current td {
		background: color-mix(in srgb, var(--accent-soft) 45%, transparent);
	}
	.copy {
		display: inline-flex;
		align-items: center;
		gap: 4px;
		font: inherit;
		color: inherit;
		background: none;
		border: 0;
		padding: 0;
		cursor: pointer;
	}
	.copy:hover {
		color: var(--accent-text);
	}
</style>
