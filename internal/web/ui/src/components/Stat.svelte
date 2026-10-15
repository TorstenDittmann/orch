<script lang="ts">
	import type { Component } from 'svelte';

	let {
		label,
		value,
		detail = '',
		icon: Icon,
		href = ''
	}: { label: string; value: string | number; detail?: string; icon?: Component<{ size?: number }>; href?: string } = $props();
</script>

<svelte:element this={href ? 'a' : 'div'} {href} class="stat panel" class:interactive={!!href}>
	<div class="top">
		<span class="label">{label}</span>
		{#if Icon}<span class="icon"><Icon size={16} /></span>{/if}
	</div>
	<div class="value">{value}</div>
	{#if detail}<div class="detail">{detail}</div>{/if}
</svelte:element>

<style>
	.stat {
		padding: 16px;
		display: flex;
		flex-direction: column;
		gap: 6px;
		min-width: 0;
		transition: border-color 0.15s, transform 0.15s;
	}
	.interactive:hover {
		border-color: var(--border-strong);
	}
	.top {
		display: flex;
		justify-content: space-between;
		align-items: center;
	}
	.label {
		color: var(--text-2);
		font-size: 13px;
		font-weight: 500;
	}
	.icon {
		color: var(--text-3);
		display: inline-flex;
	}
	.value {
		font-size: 26px;
		font-weight: 650;
		letter-spacing: -0.02em;
		line-height: 1.15;
		overflow-wrap: anywhere;
		font-variant-numeric: tabular-nums;
	}
	.detail {
		color: var(--text-3);
		font-size: 12.5px;
	}
</style>
