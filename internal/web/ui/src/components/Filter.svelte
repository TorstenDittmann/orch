<script lang="ts">
	import { Search } from '@lucide/svelte';

	let { value = $bindable(''), placeholder = 'Filter' }: { value?: string; placeholder?: string } = $props();
</script>

<label class="filter">
	<Search size={14} />
	<input bind:value {placeholder} aria-label={placeholder} />
</label>

<style>
	.filter {
		display: inline-flex;
		align-items: center;
		gap: 8px;
		height: 34px;
		padding: 0 12px;
		border: 1px solid var(--border-strong);
		border-radius: var(--radius-sm);
		background: var(--surface);
		color: var(--text-3);
		min-width: 240px;
	}
	.filter:focus-within {
		border-color: var(--accent);
		box-shadow: 0 0 0 3px var(--accent-soft);
	}
	input {
		flex: 1;
		border: 0;
		outline: 0;
		background: none;
		color: var(--text);
		font: inherit;
		font-size: 13.5px;
		min-width: 0;
	}
</style>
