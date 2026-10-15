<script lang="ts">
	import { ChevronRight, ScrollText } from '@lucide/svelte';
	import Empty from '../components/Empty.svelte';
	import Meter from '../components/Meter.svelte';
	import Status from '../components/Status.svelte';
	import { nodeActions } from '../lib/actions';
	import { ago, bytes, cores, datetime, label, ratio } from '../lib/format';
	import { href } from '../lib/router.svelte';
	import { live, openLogs } from '../lib/state.svelte';

	let { name }: { name: string } = $props();

	const node = $derived(live.snapshot?.nodes?.find((item) => item.name === name));
	const runtimes = $derived((live.snapshot?.runtimes?.runtimes ?? []).filter((runtime) => runtime.nodeName === name));
</script>

<div class="page">
	<div class="crumbs"><a href="#/nodes">Nodes</a><ChevronRight size={14} /><span>{name}</span></div>
	{#if !node}
		<section class="panel"><Empty title="Node {name} was not found" /></section>
	{:else}
		<div class="page-head">
			<div>
				<div class="row">
					<h1>{node.name}</h1>
					<Status value={node.connectionStatus} pulse />
					<Status value={node.lifecycle} />
				</div>
				<p>{node.advertiseAddress || 'No address'} · last seen {ago(node.lastSeenAt, live.now)}</p>
			</div>
			<div class="row">
				{#each nodeActions(node) as action (action.id)}
					<button class="btn" class:dangerous={action.danger} onclick={action.run}>{action.label}</button>
				{/each}
			</div>
		</div>

		{#if node.drainBlockers?.length}
			<div class="notice">Drain is blocked by {node.drainBlockers.join(', ')}.</div>
		{/if}

		<div class="grid three">
			<section class="panel pad">
				<Meter value={ratio(node.assignedCpuMillis, node.allocatableCpuMillis)} label="CPU reserved" detail="{cores(node.assignedCpuMillis)} / {cores(node.allocatableCpuMillis)} cores" />
				<p class="faint small">{cores(node.totalCpuMillis)} cores total</p>
			</section>
			<section class="panel pad">
				<Meter value={ratio(node.assignedMemoryBytes, node.allocatableMemoryBytes)} label="Memory reserved" detail="{bytes(node.assignedMemoryBytes)} / {bytes(node.allocatableMemoryBytes)}" />
				<p class="faint small">{bytes(node.totalMemoryBytes)} total</p>
			</section>
			<section class="panel pad">
				<div class="row between"><span class="muted small">Certificate</span><Status value={node.certificateState} /></div>
				<p class="faint small">Expires {datetime(node.certificateNotAfter) || 'unknown'}{node.certificateReason ? ` · ${node.certificateReason}` : ''}</p>
			</section>
		</div>

		<div class="grid two">
			<section class="panel">
				<div class="panel-head"><h2>System</h2></div>
				<dl class="kv panel-body">
					<dt>Platform</dt><dd>{node.operatingSystem}/{node.architecture}</dd>
					<dt>containerd</dt><dd>{node.runtimeVersion || '—'}</dd>
					<dt>Snapshotter</dt><dd>{node.snapshotter || '—'}</dd>
					<dt>cgroups v2</dt><dd>{node.cgroupsV2 ? 'yes' : 'no'}</dd>
					<dt>Caddy</dt><dd>{node.caddyVersion || '—'}</dd>
					<dt>Joined</dt><dd>{datetime(node.createdAt)}</dd>
					<dt>Node ID</dt><dd class="mono">{node.id}</dd>
				</dl>
			</section>
			<section class="panel">
				<div class="panel-head"><h2>Network</h2></div>
				<dl class="kv panel-body">
					<dt>Advertise address</dt><dd class="mono">{node.advertiseAddress || '—'}</dd>
					<dt>Workload subnet</dt><dd class="mono">{node.nodeSubnet || '—'}</dd>
					<dt>Network</dt><dd>{node.networkState || '—'}{node.networkReason ? ` · ${node.networkReason}` : ''}</dd>
					<dt>WireGuard port</dt><dd class="mono">{node.wireguardListenPort || '—'}</dd>
					<dt>DNS</dt><dd>{node.dnsState || '—'}</dd>
					<dt>Ingress</dt><dd>{node.ingress ? 'enabled' : 'disabled'}{node.ingressState ? ` · ${node.ingressState}` : ''}</dd>
					<dt>Labels</dt>
					<dd class="row">{#each Object.entries(node.labels ?? {}) as [key, value] (key)}<span class="chip mono">{key}={value}</span>{:else}—{/each}</dd>
				</dl>
			</section>
		</div>

		<section class="panel spaced">
			<div class="panel-head"><h2>Workloads on this node</h2><span class="hint">{runtimes.length}</span></div>
			{#if runtimes.length}
				<div class="table-wrap">
					<table class="data">
						<thead><tr><th>Application</th><th>Resource</th><th>State</th><th>Health</th><th class="num">CPU</th><th class="num">Memory</th><th>Started</th><th></th></tr></thead>
						<tbody>
							{#each runtimes as runtime (runtime.id)}
								{@const run = label(runtime.kind) === 'cron run'}
								<tr>
									<td class="primary-cell"><a href={href('apps', runtime.application)}>{runtime.application}</a></td>
									<td>{runtime.resource} <span class="faint mono">{run ? runtime.runId.slice(0, 8) : `#${runtime.replica}`}</span></td>
									<td><Status value={run ? runtime.jobRunState : runtime.allocationPhase} /></td>
									<td>{#if !run}<Status value={runtime.healthStatus} />{/if}</td>
									<td class="num mono">{cores(runtime.cpuMillis)}</td>
									<td class="num mono">{bytes(runtime.memoryBytes)}</td>
									<td class="faint">{ago(runtime.startedAt, live.now)}</td>
									<td>
										<button class="btn sm" onclick={() => openLogs(run ? { kind: 'run', app: runtime.application, job: runtime.resource, run: runtime.runId } : { kind: 'service', app: runtime.application, service: runtime.resource, replica: runtime.replica })}>
											<ScrollText size={13} /> Logs
										</button>
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{:else}
				<Empty title="Nothing is running here" />
			{/if}
		</section>
	{/if}
</div>

<style>
	.three {
		grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
		margin-bottom: 16px;
	}
	.two {
		grid-template-columns: repeat(auto-fit, minmax(380px, 1fr));
		align-items: start;
	}
	.pad {
		padding: 16px;
	}
	.small {
		font-size: 12.5px;
		margin: 8px 0 0;
	}
	.between {
		justify-content: space-between;
	}
	.spaced {
		margin-top: 16px;
	}
	.notice {
		padding: 12px 16px;
		margin-bottom: 16px;
		border-radius: var(--radius);
		background: var(--warn-soft);
		color: var(--warn);
	}
	.dangerous {
		color: var(--bad);
	}
</style>
