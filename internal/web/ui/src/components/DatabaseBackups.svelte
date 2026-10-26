<script lang="ts">
	import { Archive, RotateCcw } from '@lucide/svelte';
	import { onDestroy } from 'svelte';
	import { api } from '../lib/api';
	import { ago, bytes, datetime } from '../lib/format';
	import { ask, live, toast } from '../lib/state.svelte';
	import type { DatabaseBackups, VolumeBackup } from '../lib/types';
	import Empty from './Empty.svelte';

	let { app, database }: { app: string; database: string } = $props();

	let data = $state<DatabaseBackups | null>(null);
	let error = $state('');
	let backingUp = $state(false);

	let timer: ReturnType<typeof setTimeout> | undefined;
	async function load() {
		clearTimeout(timer);
		const requested = database;
		try {
			const value = await api.databaseBackups(app, requested);
			if (requested !== database) return;
			data = value;
			error = '';
		} catch (err) {
			if (requested !== database) return;
			error = (err as Error).message;
		}
		// Scheduled backups land on their own; a slow poll picks them up.
		timer = setTimeout(load, document.hidden ? 60000 : 20000);
	}
	$effect(() => {
		void database;
		data = null;
		error = '';
		load();
	});
	onDestroy(() => clearTimeout(timer));

	const volume = $derived(data?.volume ?? null);
	const backups = $derived(volume?.backups ?? []);
	const total = $derived(backups.reduce((sum, backup) => sum + Number(backup.sizeBytes || 0), 0));

	async function backup() {
		backingUp = true;
		try {
			const created = await api.backupDatabase(app, database);
			toast(`Backed up ${database} as ${created.name} (${bytes(created.sizeBytes)})`, 'ok');
			load();
		} catch (err) {
			toast((err as Error).message, 'bad');
		} finally {
			backingUp = false;
		}
	}

	function restore(item: VolumeBackup) {
		const target = database;
		const when = `${datetime(item.createdAt)} (${ago(item.createdAt, live.now)})`;
		ask({
			title: `Restore ${target} from ${item.name}?`,
			body: data?.database.keepsPreviousData
				? `${target} restarts with the data from ${when}.\nIts current data is kept as a backup first, so you can restore that to undo.`
				: `${target} restarts and the dump from ${when} is loaded into it, replacing its current data. This can take a few minutes.\nBack up first if you may want to return to the current data.`,
			action: 'Restore',
			danger: true,
			success: `Restored ${target} from ${item.name}`,
			run: async () => {
				await api.restoreDatabase(app, target, item.name);
				load();
			}
		});
	}

	function stored(item: VolumeBackup): string {
		if (item.onNode && item.inBucket) return 'node + bucket';
		return item.inBucket ? 'bucket' : 'node';
	}
</script>

<section class="panel spaced">
	<div class="panel-head">
		<div class="row">
			<h2>Backups of {database}</h2>
			{#if backups.length}<span class="hint">{backups.length} {backups.length === 1 ? 'backup' : 'backups'} · {bytes(total)}</span>{/if}
		</div>
		{#if volume}
			<button class="btn sm" disabled={backingUp} onclick={backup}><Archive size={13} /> {backingUp ? 'Backing up…' : 'Back up now'}</button>
		{/if}
	</div>
	{#if error && !data}
		<Empty title="Could not load backups">{error}</Empty>
	{:else if !data}
		<Empty title="Loading…" />
	{:else if !data.database.backupVolume}
		<Empty title="Backups are off for {database}">
			Add <span class="mono">backup = new DatabaseBackup {'{}'}</span> to it in orch.pkl to turn them on.
		</Empty>
	{:else}
		<dl class="kv panel-body">
			<dt>Schedule</dt>
			<dd><span class="mono">{data.database.backupSchedule}</span> <span class="faint">· keeps {data.database.backupKeep}</span></dd>
			<dt>Stored</dt>
			<dd>
				{#if volume?.node}on node {volume.node}{#if !volume.reachable} <span class="warn">(unreachable)</span>{/if}{:else}<span class="faint">not on a node yet</span>{/if}
				{#if data.destination}
					· off-host in <span class="mono">{data.destination}</span>
				{:else}
					<span class="faint">· no off-host copy; set one with <span class="mono">orch backup destination set</span></span>
				{/if}
			</dd>
			<dt>Volume</dt>
			<dd class="mono faint">{data.database.backupVolume}</dd>
		</dl>
		{#if volume?.backupFailure}
			<div class="alert">Last scheduled backup failed {ago(volume.backupFailedAt, live.now)}: {volume.backupFailure}</div>
		{/if}
		{#if volume?.bucketError}
			<div class="alert">Could not list the copies in the bucket: {volume.bucketError}</div>
		{/if}
		{#if backups.length}
			<div class="table-wrap">
				<table class="data">
					<thead><tr><th>Backup</th><th>Created</th><th class="num">Size</th><th>Stored</th><th></th></tr></thead>
					<tbody>
						{#each backups as item (item.name)}
							<tr>
								<td class="primary-cell mono">
									{item.name}
									{#if item.name.endsWith('-before-restore')}<span class="chip">before restore</span>{/if}
								</td>
								<td title={datetime(item.createdAt)}>{ago(item.createdAt, live.now)}</td>
								<td class="num">{bytes(item.sizeBytes)}</td>
								<td class="faint">{stored(item)}</td>
								<td><button class="btn sm" onclick={() => restore(item)}><RotateCcw size={13} /> Restore</button></td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{:else}
			<Empty title="No backups yet">Back up now takes one; scheduled backups appear here too.</Empty>
		{/if}
	{/if}
</section>

<style>
	.spaced {
		margin-top: 16px;
	}
	.warn {
		color: var(--warn);
	}
	.alert {
		margin: 0 16px 12px;
		padding: 10px 12px;
		border-radius: var(--radius);
		background: var(--bad-soft);
		color: var(--bad);
		font-size: 13px;
	}
</style>
