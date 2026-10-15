<script lang="ts">
	import { CircleAlert, CircleCheck, Info, X } from '@lucide/svelte';
	import { fly } from 'svelte/transition';
	import { dismiss, toasts } from '../lib/state.svelte';

	const icons = { ok: CircleCheck, bad: CircleAlert, info: Info };
</script>

<div class="toasts" aria-live="polite">
	{#each toasts as item (item.id)}
		{@const Icon = icons[item.tone]}
		<div class="toast {item.tone}" transition:fly={{ y: 12, duration: 180 }}>
			<Icon size={16} />
			<span>{item.text}</span>
			<button aria-label="Dismiss" onclick={() => dismiss(item.id)}><X size={14} /></button>
		</div>
	{/each}
</div>

<style>
	.toasts {
		position: fixed;
		right: 20px;
		bottom: 20px;
		display: flex;
		flex-direction: column;
		gap: 8px;
		z-index: 60;
		max-width: min(420px, calc(100vw - 40px));
	}
	.toast {
		display: flex;
		align-items: flex-start;
		gap: 10px;
		padding: 12px 12px 12px 14px;
		border-radius: var(--radius);
		background: var(--surface);
		border: 1px solid var(--border);
		box-shadow: var(--shadow-lg);
		font-size: 13.5px;
	}
	.toast :global(svg:first-child) {
		flex: none;
		margin-top: 2px;
	}
	.ok :global(svg:first-child) {
		color: var(--ok);
	}
	.bad :global(svg:first-child) {
		color: var(--bad);
	}
	.info :global(svg:first-child) {
		color: var(--info);
	}
	span {
		flex: 1;
		overflow-wrap: anywhere;
	}
	button {
		border: 0;
		background: none;
		color: var(--text-3);
		cursor: pointer;
		padding: 2px;
		display: inline-flex;
	}
</style>
