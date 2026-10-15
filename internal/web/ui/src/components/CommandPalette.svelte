<script lang="ts">
	import { Activity, AppWindow, Boxes, Briefcase, CornerDownLeft, LayoutDashboard, Moon, ScrollText, Server, Sun } from '@lucide/svelte';
	import type { Component } from 'svelte';
	import { label } from '../lib/format';
	import { href, navigate } from '../lib/router.svelte';
	import { applyTheme, live, openLogs, ui } from '../lib/state.svelte';
	import type { LogSource } from '../lib/types';

	interface Item {
		id: string;
		group: string;
		title: string;
		hint: string;
		icon: Component<{ size?: number }>;
		run: () => void;
	}

	let query = $state('');
	let active = $state(0);
	let dialog = $state<HTMLDialogElement>();
	let input = $state<HTMLInputElement>();
	let list = $state<HTMLDivElement>();

	const go = (target: string) => () => navigate(target);
	const logs = (source: LogSource) => () => openLogs(source);

	const items = $derived.by(() => {
		const snapshot = live.snapshot;
		const result: Item[] = [
			{ id: 'p-overview', group: 'Pages', title: 'Overview', hint: '', icon: LayoutDashboard, run: go('#/') },
			{ id: 'p-apps', group: 'Pages', title: 'Applications', hint: '', icon: AppWindow, run: go('#/apps') },
			{ id: 'p-runtimes', group: 'Pages', title: 'Runtimes', hint: '', icon: Boxes, run: go('#/runtimes') },
			{ id: 'p-nodes', group: 'Pages', title: 'Nodes', hint: '', icon: Server, run: go('#/nodes') },
			{ id: 'p-jobs', group: 'Pages', title: 'Jobs', hint: '', icon: Briefcase, run: go('#/jobs') },
			{ id: 'p-events', group: 'Pages', title: 'Events', hint: '', icon: Activity, run: go('#/events') }
		];
		for (const app of snapshot?.applications ?? []) {
			result.push({ id: `a-${app.name}`, group: 'Applications', title: app.name, hint: label(app.executionStatus), icon: AppWindow, run: go(href('apps', app.name)) });
		}
		for (const node of snapshot?.nodes ?? []) {
			result.push({ id: `n-${node.name}`, group: 'Nodes', title: node.name, hint: label(node.connectionStatus), icon: Server, run: go(href('nodes', node.name)) });
		}
		for (const runtime of snapshot?.runtimes?.runtimes ?? []) {
			const run = label(runtime.kind) === 'cron run';
			result.push({
				id: `l-${runtime.id}`,
				group: 'Logs',
				title: run ? `${runtime.application} / ${runtime.resource} run` : `${runtime.application} / ${runtime.resource} #${runtime.replica}`,
				hint: runtime.nodeName,
				icon: ScrollText,
				run: logs(run ? { kind: 'run', app: runtime.application, job: runtime.resource, run: runtime.runId } : { kind: 'service', app: runtime.application, service: runtime.resource, replica: runtime.replica })
			});
		}
		result.push(
			{ id: 't-light', group: 'Preferences', title: 'Use light theme', hint: '', icon: Sun, run: () => applyTheme('light') },
			{ id: 't-dark', group: 'Preferences', title: 'Use dark theme', hint: '', icon: Moon, run: () => applyTheme('dark') }
		);
		return result;
	});

	function score(item: Item, terms: string[]): number {
		const haystack = `${item.title} ${item.group} ${item.hint}`.toLowerCase();
		let total = 0;
		for (const term of terms) {
			const index = haystack.indexOf(term);
			if (index < 0) return -1;
			total += index === 0 ? 3 : haystack[index - 1] === ' ' || haystack[index - 1] === '/' ? 2 : 1;
		}
		return total;
	}

	const results = $derived.by(() => {
		const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
		if (!terms.length) return items.filter((item) => item.group !== 'Logs').slice(0, 40);
		return items
			.map((item) => ({ item, rank: score(item, terms) }))
			.filter((entry) => entry.rank >= 0)
			.sort((a, b) => b.rank - a.rank)
			.slice(0, 40)
			.map((entry) => entry.item);
	});

	$effect(() => {
		void query;
		active = 0;
	});

	$effect(() => {
		if (!dialog) return;
		if (ui.palette && !dialog.open) {
			query = '';
			dialog.showModal();
			input?.focus();
		}
		if (!ui.palette && dialog.open) dialog.close();
	});

	function choose(item: Item | undefined) {
		if (!item) return;
		ui.palette = false;
		item.run();
	}

	function onkeydown(event: KeyboardEvent) {
		if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
			event.preventDefault();
			const step = event.key === 'ArrowDown' ? 1 : -1;
			active = (active + step + results.length) % Math.max(results.length, 1);
			list?.querySelector(`[data-index="${active}"]`)?.scrollIntoView({ block: 'nearest' });
		} else if (event.key === 'Enter') {
			event.preventDefault();
			choose(results[active]);
		}
	}

	function onwindowkey(event: KeyboardEvent) {
		if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
			event.preventDefault();
			ui.palette = !ui.palette;
		} else if (event.key === '/' && !ui.palette && !(event.target instanceof HTMLInputElement) && !(event.target instanceof HTMLTextAreaElement)) {
			event.preventDefault();
			ui.palette = true;
		}
	}
</script>

<svelte:window onkeydown={onwindowkey} />

<dialog bind:this={dialog} onclose={() => (ui.palette = false)} onclick={(event) => event.target === dialog && (ui.palette = false)}>
	<div class="palette">
		<input bind:this={input} bind:value={query} {onkeydown} placeholder="Jump to an application, node, logs…" aria-label="Search" />
		<div class="list" bind:this={list} role="listbox">
			{#each results as item, index (item.id)}
				{#if index === 0 || results[index - 1].group !== item.group}
					<div class="group">{item.group}</div>
				{/if}
				<button
					class="item"
					class:active={index === active}
					role="option"
					aria-selected={index === active}
					data-index={index}
					onmousemove={() => (active = index)}
					onclick={() => choose(item)}
				>
					<item.icon size={16} />
					<span class="title">{item.title}</span>
					<span class="hint">{item.hint}</span>
					{#if index === active}<CornerDownLeft size={14} />{/if}
				</button>
			{:else}
				<div class="none">No results for “{query}”</div>
			{/each}
		</div>
		<footer><span><kbd>↑</kbd> <kbd>↓</kbd> navigate</span><span><kbd>↵</kbd> open</span><span><kbd>esc</kbd> close</span></footer>
	</div>
</dialog>

<style>
	dialog {
		border: 0;
		padding: 0;
		margin: 12vh auto auto;
		width: min(640px, calc(100vw - 32px));
		border-radius: 14px;
		background: var(--surface);
		color: var(--text);
		box-shadow: var(--shadow-lg);
	}
	dialog::backdrop {
		background: rgb(10 12 16 / 0.45);
		backdrop-filter: blur(2px);
	}
	input {
		width: 100%;
		border: 0;
		border-bottom: 1px solid var(--border);
		padding: 18px 20px;
		font: inherit;
		font-size: 16px;
		background: none;
		color: var(--text);
		outline: none;
	}
	.list {
		max-height: min(420px, 60vh);
		overflow: auto;
		padding: 6px;
	}
	.group {
		font-size: 11.5px;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-3);
		padding: 10px 12px 4px;
	}
	.item {
		display: flex;
		align-items: center;
		gap: 10px;
		width: 100%;
		height: 38px;
		padding: 0 12px;
		border: 0;
		border-radius: 8px;
		background: none;
		cursor: pointer;
		text-align: left;
		color: var(--text-2);
	}
	.item.active {
		background: var(--accent-soft);
		color: var(--accent-text);
	}
	.title {
		color: var(--text);
		font-weight: 500;
		flex: 1;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.hint {
		font-size: 12.5px;
		color: var(--text-3);
	}
	.none {
		padding: 28px;
		text-align: center;
		color: var(--text-3);
	}
	footer {
		display: flex;
		gap: 16px;
		padding: 10px 16px;
		border-top: 1px solid var(--border);
		font-size: 12px;
		color: var(--text-3);
	}
</style>
