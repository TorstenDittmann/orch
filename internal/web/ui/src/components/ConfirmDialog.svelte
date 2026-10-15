<script lang="ts">
	import { TriangleAlert } from '@lucide/svelte';
	import { confirmPending, ui } from '../lib/state.svelte';

	let dialog = $state<HTMLDialogElement>();

	$effect(() => {
		if (!dialog) return;
		if (ui.confirm && !dialog.open) dialog.showModal();
		if (!ui.confirm && dialog.open) dialog.close();
	});
</script>

<dialog bind:this={dialog} onclose={() => (ui.confirm = null)}>
	{#if ui.confirm}
		<form method="dialog" onsubmit={(event) => { event.preventDefault(); confirmPending(); }}>
			<div class="head">
				{#if ui.confirm.danger}<span class="warn"><TriangleAlert size={18} /></span>{/if}
				<h2>{ui.confirm.title}</h2>
			</div>
			<p>{ui.confirm.body}</p>
			<div class="actions">
				<button type="button" class="btn" onclick={() => (ui.confirm = null)} disabled={ui.busy}>Cancel</button>
				<button type="submit" class="btn {ui.confirm.danger ? 'danger' : 'primary'}" disabled={ui.busy}>
					{ui.busy ? 'Working…' : ui.confirm.action}
				</button>
			</div>
		</form>
	{/if}
</dialog>

<style>
	dialog {
		border: 0;
		padding: 0;
		border-radius: 14px;
		background: var(--surface);
		color: var(--text);
		width: min(440px, calc(100vw - 32px));
		box-shadow: var(--shadow-lg);
	}
	dialog::backdrop {
		background: rgb(10 12 16 / 0.45);
		backdrop-filter: blur(2px);
	}
	form {
		padding: 22px;
	}
	.head {
		display: flex;
		align-items: center;
		gap: 10px;
	}
	h2 {
		font-size: 16px;
		font-weight: 600;
	}
	.warn {
		color: var(--bad);
		display: inline-flex;
	}
	p {
		color: var(--text-2);
		margin: 10px 0 22px;
		white-space: pre-line;
	}
	.actions {
		display: flex;
		justify-content: flex-end;
		gap: 8px;
	}
</style>
