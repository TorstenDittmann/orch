<script lang="ts">
	import type { Snippet } from 'svelte';

	let { title, children }: { title: string; children?: Snippet } = $props();
</script>

<div class="empty">
	<div class="title">{title}</div>
	{#if children}<div class="body">{@render children()}</div>{/if}
</div>

<style>
	.title {
		color: var(--text-2);
		font-weight: 550;
	}
	.body {
		margin-top: 4px;
		font-size: 13px;
	}
</style>
