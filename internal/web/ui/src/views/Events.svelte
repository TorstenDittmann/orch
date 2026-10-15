<script lang="ts">
	import { ChevronDown, ChevronRight } from '@lucide/svelte';
	import Empty from '../components/Empty.svelte';
	import Filter from '../components/Filter.svelte';
	import { ago, datetime, pretty } from '../lib/format';
	import { live } from '../lib/state.svelte';

	let query = $state('');
	let kind = $state('');
	let open = $state<Record<string, boolean>>({});

	const events = $derived([...(live.snapshot?.events?.events ?? [])].reverse());
	const kinds = $derived([...new Set(events.map((event) => event.kind))].sort());
	const filtered = $derived(
		events.filter((event) => (!kind || event.kind === kind) && (!query || `${event.kind} ${event.payloadJson}`.toLowerCase().includes(query.toLowerCase())))
	);

	function summary(json: string): string {
		try {
			const value = JSON.parse(json) as Record<string, unknown>;
			return Object.entries(value)
				.filter(([, item]) => item !== null && typeof item !== 'object')
				.slice(0, 4)
				.map(([key, item]) => `${key}=${item}`)
				.join('  ');
		} catch {
			return json;
		}
	}
</script>

<div class="page">
	<div class="page-head">
		<div>
			<h1>Events</h1>
			<p>The controller's audit trail of deployments, placements, and node changes. Showing the latest {events.length}.</p>
		</div>
	</div>

	<div class="row toolbar">
		<Filter bind:value={query} placeholder="Search events" />
		<select class="input" bind:value={kind} aria-label="Event kind">
			<option value="">All kinds</option>
			{#each kinds as item (item)}<option value={item}>{item}</option>{/each}
		</select>
	</div>

	<section class="panel">
		{#if filtered.length}
			<ul class="events">
				{#each filtered as event (event.sequence)}
					<li>
						<button class="head" onclick={() => (open[event.sequence] = !open[event.sequence])} aria-expanded={!!open[event.sequence]}>
							{#if open[event.sequence]}<ChevronDown size={14} />{:else}<ChevronRight size={14} />{/if}
							<span class="seq mono faint">#{event.sequence}</span>
							<span class="kind mono">{event.kind}</span>
							<span class="summary faint mono">{summary(event.payloadJson)}</span>
							<span class="when faint" title={datetime(event.createdAt)}>{ago(event.createdAt, live.now)}</span>
						</button>
						{#if open[event.sequence]}
							<pre class="code">{pretty(event.payloadJson)}</pre>
						{/if}
					</li>
				{/each}
			</ul>
		{:else}
			<Empty title={events.length ? 'No events match' : 'No events yet'} />
		{/if}
	</section>
</div>

<style>
	.toolbar {
		margin-bottom: 16px;
	}
	.events {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.events li + li {
		border-top: 1px solid var(--border);
	}
	.head {
		display: flex;
		align-items: center;
		gap: 10px;
		width: 100%;
		padding: 11px 16px;
		border: 0;
		background: none;
		cursor: pointer;
		text-align: left;
		color: var(--text-3);
	}
	.head:hover {
		background: var(--surface-2);
	}
	.seq {
		width: 56px;
		flex: none;
	}
	.kind {
		color: var(--text);
		font-weight: 500;
		flex: none;
	}
	.summary {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: 12px;
	}
	.when {
		flex: none;
		font-size: 12.5px;
	}
	pre {
		margin: 0 16px 14px 40px;
		border-radius: var(--radius-sm);
	}
</style>
