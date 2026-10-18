<script lang="ts">
	import { ArrowDownToLine, Download, Eraser, Pause, Play, Search, WrapText, X } from '@lucide/svelte';
	import { untrack } from 'svelte';
	import { fly, fade } from 'svelte/transition';
	import { api } from '../lib/api';
	import Segmented from './Segmented.svelte';
	import { ui } from '../lib/state.svelte';
	import type { LogLine, LogSource } from '../lib/types';

	const maxLines = 10000;

	let lines = $state<LogLine[]>([]);
	let status = $state<'loading' | 'following' | 'paused' | 'exited' | 'error' | 'previous'>('loading');
	let error = $state('');
	let filter = $state('');
	let wrap = $state(true);
	let paused = $state(false);
	let atBottom = $state(true);
	let body = $state<HTMLDivElement>();
	let instance = $state('current');

	const source = $derived(ui.log);
	const title = $derived(
		source ? (source.kind === 'service' ? `${source.service} #${source.replica}` : source.label || `${source.job} · ${source.run}`) : ''
	);
	const visible = $derived.by(() => {
		const query = filter.trim().toLowerCase();
		return query ? lines.filter((line) => line.text.toLowerCase().includes(query)) : lines;
	});

	$effect(() => {
		void source;
		untrack(() => (instance = 'current'));
	});

	const previous = $derived(source?.kind === 'service' && instance === 'previous');

	$effect(() => {
		const current = source;
		if (!current) return;
		const earlier = previous;
		let cursor = '';
		let stopped = false;
		let timer: ReturnType<typeof setTimeout> | undefined;
		lines = [];
		status = 'loading';
		error = '';
		atBottom = true;

		async function poll(target: LogSource) {
			if (stopped) return;
			if (paused) {
				status = 'paused';
				timer = setTimeout(() => poll(target), 1000);
				return;
			}
			try {
				const batch = await api.logs(target, cursor);
				if (stopped) return;
				const next = batch.cursorReset ? [...lines, { stream: 'orch', text: '— log rotated or reset —' }] : [...lines];
				for (const line of batch.lines) {
					for (const text of line.text.replace(/\n$/, '').split('\n')) next.push({ stream: line.stream, text });
				}
				lines = next.length > maxLines ? next.slice(next.length - maxLines) : next;
				cursor = batch.nextCursor || cursor;
				error = '';
				if (batch.exited) {
					status = 'exited';
					return;
				}
				status = 'following';
			} catch (err) {
				if (stopped) return;
				error = (err as Error).message;
				status = 'error';
			}
			timer = setTimeout(() => poll(target), status === 'error' ? 4000 : 1500);
		}

		async function loadPrevious(target: LogSource) {
			try {
				const batch = await api.logs(target, '', true);
				if (stopped) return;
				const next: LogLine[] = [];
				for (const line of batch.lines) {
					for (const text of line.text.replace(/\n$/, '').split('\n')) next.push({ stream: line.stream, text });
				}
				lines = next.length > maxLines ? next.slice(next.length - maxLines) : next;
				status = 'previous';
			} catch (err) {
				if (stopped) return;
				error = (err as Error).message;
				status = 'error';
			}
		}

		untrack(() => (earlier ? loadPrevious(current) : poll(current)));
		return () => {
			stopped = true;
			clearTimeout(timer);
		};
	});

	$effect(() => {
		void visible.length;
		if (body && atBottom) queueMicrotask(() => body && (body.scrollTop = body.scrollHeight));
	});

	function onscroll() {
		if (!body) return;
		atBottom = body.scrollTop + body.clientHeight >= body.scrollHeight - 8;
	}

	function jump() {
		atBottom = true;
		if (body) body.scrollTop = body.scrollHeight;
	}

	function download() {
		const blob = new Blob([lines.map((line) => line.text).join('\n') + '\n'], { type: 'text/plain' });
		const link = document.createElement('a');
		link.href = URL.createObjectURL(blob);
		link.download = `${(title || 'logs').replace(/[^\w.-]+/g, '-')}.log`;
		link.click();
		setTimeout(() => URL.revokeObjectURL(link.href), 1000);
	}

	function close() {
		ui.log = null;
	}

	function onkeydown(event: KeyboardEvent) {
		if (event.key === 'Escape' && ui.log && !ui.palette && !ui.confirm) close();
	}
</script>

<svelte:window {onkeydown} />

{#if source}
	<button class="scrim" aria-label="Close logs" onclick={close} transition:fade={{ duration: 150 }}></button>
	<section class="drawer" aria-label="Logs" transition:fly={{ x: 40, duration: 200 }}>
		<header>
			<div class="title">
				<div class="eyebrow">{source.app} · {source.kind === 'service' ? 'service logs' : 'run logs'}</div>
				<h2>{title}</h2>
			</div>
			<span class="state {status}">
				<span class="dot"></span>{status === 'loading' ? (previous ? 'Loading' : 'Connecting') : status === 'following' ? 'Live' : status === 'previous' ? 'Previous instance' : previous && status === 'error' ? 'Unavailable' : status}
			</span>
			<button class="btn icon ghost" aria-label="Close" onclick={close}><X size={18} /></button>
		</header>
		<div class="toolbar">
			{#if source.kind === 'service'}
				<Segmented options={[{ value: 'current', label: 'Current' }, { value: 'previous', label: 'Previous instance' }]} bind:value={instance} />
			{/if}
			<label class="filter">
				<Search size={14} />
				<input placeholder="Filter lines" bind:value={filter} />
				{#if filter}<span class="faint">{visible.length}/{lines.length}</span>{/if}
			</label>
			<button class="btn sm ghost" class:on={wrap} title="Wrap lines" aria-pressed={wrap} onclick={() => (wrap = !wrap)}><WrapText size={14} /></button>
			<button class="btn sm ghost" title={paused ? 'Resume' : 'Pause'} disabled={previous} onclick={() => (paused = !paused)}>
				{#if paused}<Play size={14} />{:else}<Pause size={14} />{/if}
			</button>
			<button class="btn sm ghost" title="Clear" onclick={() => (lines = [])}><Eraser size={14} /></button>
			<button class="btn sm ghost" title="Download" onclick={download}><Download size={14} /></button>
		</div>
		<div class="body" class:wrap bind:this={body} {onscroll}>
			{#if previous && (error || (status === 'previous' && !lines.length))}
				<div class="quiet">
					<p>No earlier instance logs kept for this replica (kept 24 hours).</p>
					{#if error}<p class="reason">{error}</p>{/if}
				</div>
			{:else if error}
				<div class="banner">{error}</div>
			{/if}
			{#if !visible.length && !(previous && (error || status === 'previous'))}
				<div class="quiet">{status === 'loading' ? 'Loading logs…' : filter ? 'No matching lines.' : 'No output yet.'}</div>
			{/if}
			{#each visible as line, index (index)}
				<div class="line {line.stream}"><span class="n">{index + 1}</span><span class="t">{line.text}</span></div>
			{/each}
		</div>
		{#if !atBottom}
			<button class="jump btn sm" onclick={jump} transition:fade={{ duration: 120 }}><ArrowDownToLine size={14} /> Latest</button>
		{/if}
	</section>
{/if}

<style>
	.scrim {
		position: fixed;
		inset: 0;
		border: 0;
		background: rgb(10 12 16 / 0.35);
		z-index: 40;
	}
	.drawer {
		position: fixed;
		top: 0;
		right: 0;
		bottom: 0;
		width: min(980px, 100vw);
		background: var(--surface);
		border-left: 1px solid var(--border);
		box-shadow: var(--shadow-lg);
		z-index: 41;
		display: flex;
		flex-direction: column;
	}
	header {
		display: flex;
		align-items: center;
		gap: 12px;
		padding: 16px 16px 12px 20px;
	}
	.title {
		flex: 1;
		min-width: 0;
	}
	.eyebrow {
		font-size: 12px;
		color: var(--text-3);
	}
	h2 {
		font-size: 16px;
		font-weight: 600;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.state {
		display: inline-flex;
		align-items: center;
		gap: 6px;
		font-size: 12px;
		color: var(--text-2);
		text-transform: capitalize;
	}
	.dot {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: var(--neutral);
	}
	.following .dot {
		background: var(--ok);
		animation: blink 1.6s infinite;
	}
	.error .dot {
		background: var(--bad);
	}
	.paused .dot,
	.loading .dot {
		background: var(--warn);
	}
	@keyframes blink {
		50% {
			opacity: 0.35;
		}
	}
	.toolbar {
		display: flex;
		align-items: center;
		gap: 4px;
		padding: 0 16px 12px 20px;
		border-bottom: 1px solid var(--border);
	}
	.filter {
		flex: 1;
		display: flex;
		align-items: center;
		gap: 8px;
		height: 32px;
		padding: 0 10px;
		border: 1px solid var(--border-strong);
		border-radius: var(--radius-sm);
		color: var(--text-3);
		margin-right: 6px;
	}
	.filter:focus-within {
		border-color: var(--accent);
		box-shadow: 0 0 0 3px var(--accent-soft);
	}
	.filter input {
		flex: 1;
		border: 0;
		outline: 0;
		background: none;
		color: var(--text);
		font: inherit;
		font-size: 13px;
		min-width: 0;
	}
	.on {
		background: var(--accent-soft);
		color: var(--accent-text);
	}
	.body {
		flex: 1;
		overflow: auto;
		background: var(--code-bg);
		color: var(--code-text);
		font: 12.5px/1.6 var(--mono);
		padding: 10px 0 24px;
	}
	.line {
		display: flex;
		padding: 0 16px 0 0;
		white-space: pre;
	}
	.wrap .line {
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
	.line:hover {
		background: rgb(255 255 255 / 0.04);
	}
	.n {
		flex: none;
		width: 56px;
		padding-right: 14px;
		text-align: right;
		color: #4b5160;
		user-select: none;
	}
	.t {
		min-width: 0;
	}
	.stderr .t {
		color: #ff8b8b;
	}
	.orch .t {
		color: #8b93ff;
		font-style: italic;
	}
	.quiet {
		color: #6b7280;
		padding: 24px;
		font-family: var(--font);
	}
	.quiet p {
		margin: 0;
	}
	.quiet .reason {
		margin-top: 6px;
		font: 12px/1.5 var(--mono);
		color: #4b5160;
	}
	.toolbar :global(.segmented) {
		margin-right: 6px;
	}
	.banner {
		margin: 0 16px 10px;
		padding: 8px 12px;
		border-radius: var(--radius-sm);
		background: #3a1518;
		color: #ffb4b4;
		font-family: var(--font);
	}
	.jump {
		position: absolute;
		right: 28px;
		bottom: 24px;
		box-shadow: var(--shadow-lg);
	}
</style>
