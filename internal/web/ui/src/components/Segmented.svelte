<script lang="ts">
	let { options, value = $bindable() }: { options: { value: string; label: string; count?: number }[]; value: string } = $props();
</script>

<div class="segmented" role="radiogroup">
	{#each options as option (option.value)}
		<button role="radio" aria-checked={value === option.value} class:on={value === option.value} onclick={() => (value = option.value)}>
			{option.label}
			{#if option.count !== undefined}<span class="count">{option.count}</span>{/if}
		</button>
	{/each}
</div>

<style>
	.segmented {
		display: inline-flex;
		padding: 3px;
		gap: 2px;
		background: var(--surface-2);
		border: 1px solid var(--border);
		border-radius: 8px;
	}
	button {
		border: 0;
		background: none;
		height: 26px;
		padding: 0 10px;
		border-radius: 6px;
		font-size: 13px;
		font-weight: 500;
		color: var(--text-2);
		cursor: pointer;
		display: inline-flex;
		align-items: center;
		gap: 6px;
	}
	button.on {
		background: var(--surface);
		color: var(--text);
		box-shadow: var(--shadow);
	}
	.count {
		font-size: 11.5px;
		color: var(--text-3);
	}
</style>
