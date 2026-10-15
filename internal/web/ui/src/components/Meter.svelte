<script lang="ts">
	let { value, label = '', detail = '' }: { value: number; label?: string; detail?: string } = $props();
	const level = $derived(value >= 0.9 ? 'bad' : value >= 0.75 ? 'warn' : 'ok');
</script>

<div class="meter">
	{#if label || detail}
		<div class="meta"><span>{label}</span><span class="faint">{detail}</span></div>
	{/if}
	<div class="track" role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(value * 100)} aria-label={label}>
		<div class="fill {level}" style:width="{Math.max(value * 100, value > 0 ? 2 : 0)}%"></div>
	</div>
</div>

<style>
	.meter {
		min-width: 120px;
	}
	.meta {
		display: flex;
		justify-content: space-between;
		gap: 8px;
		font-size: 12px;
		margin-bottom: 5px;
		color: var(--text-2);
	}
	.track {
		height: 6px;
		border-radius: 999px;
		background: var(--surface-3);
		overflow: hidden;
	}
	.fill {
		height: 100%;
		border-radius: inherit;
		transition: width 0.4s ease;
	}
	.ok {
		background: var(--accent);
	}
	.warn {
		background: var(--warn);
	}
	.bad {
		background: var(--bad);
	}
</style>
