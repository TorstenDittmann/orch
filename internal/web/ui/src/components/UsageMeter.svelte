<script lang="ts">
	// UsageMeter shows what is actually in use as the bar and what is
	// reserved as a quiet band behind it. A fully reserved node is normal,
	// so only real usage turns the bar amber or red.
	let {
		label,
		used,
		reserved,
		total,
		format,
		unit = ''
	}: {
		label: string;
		used?: string | number | null;
		reserved?: string | number | null;
		total: string | number;
		format: (value: string | number | undefined) => string;
		unit?: string;
	} = $props();

	const scale = $derived(Number(total || 0));
	const part = (value: string | number | null | undefined) => (scale > 0 && value != null ? Math.min(1, Number(value) / scale) : 0);
	const usedRatio = $derived(part(used));
	const reservedRatio = $derived(part(reserved));
	const level = $derived(usedRatio >= 0.9 ? 'bad' : usedRatio >= 0.75 ? 'warn' : 'ok');
	const suffix = $derived(unit ? ` ${unit}` : '');
</script>

<div class="meter">
	<div class="meta">
		<span>{label}</span>
		<span class="faint">
			{#if used != null}<strong>{format(used)}</strong> used{:else}no recent report{/if}
			/ {format(total)}{suffix}
		</span>
	</div>
	<div class="track" role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(usedRatio * 100)} aria-label="{label} in use">
		{#if reserved != null}<div class="reserved" style:width="{reservedRatio * 100}%"></div>{/if}
		{#if used != null}<div class="fill {level}" style:width="{Math.max(usedRatio * 100, usedRatio > 0 ? 2 : 0)}%"></div>{/if}
	</div>
	{#if reserved != null}
		<div class="legend faint"><span class="swatch"></span>{format(reserved)}{suffix} reserved ({Math.round(reservedRatio * 100)}%)</div>
	{/if}
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
		font-variant-numeric: tabular-nums;
	}
	.meta strong {
		color: var(--text);
		font-weight: 600;
	}
	.track {
		position: relative;
		height: 6px;
		border-radius: 999px;
		background: var(--surface-3);
		overflow: hidden;
	}
	.reserved,
	.fill {
		position: absolute;
		inset: 0 auto 0 0;
		border-radius: inherit;
		transition: width 0.4s ease;
	}
	.reserved {
		background: color-mix(in srgb, var(--text-3) 35%, transparent);
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
	.legend {
		display: flex;
		align-items: center;
		gap: 6px;
		margin-top: 5px;
		font-size: 11.5px;
		font-variant-numeric: tabular-nums;
	}
	.swatch {
		width: 8px;
		height: 8px;
		border-radius: 2px;
		background: color-mix(in srgb, var(--text-3) 35%, transparent);
	}
</style>
