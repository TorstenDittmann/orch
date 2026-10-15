<script lang="ts">
	import { label, tone } from '../lib/format';

	let { value, text = '', pulse = false }: { value: string | undefined | null; text?: string; pulse?: boolean } = $props();
	const kind = $derived(tone(value));
</script>

<span class="status {kind}" class:pulse={pulse && (kind === 'ok' || kind === 'warn')}>
	<span class="dot"></span>{text || label(value) || 'unknown'}
</span>

<style>
	.status {
		display: inline-flex;
		align-items: center;
		gap: 6px;
		height: 22px;
		padding: 0 8px 0 7px;
		border-radius: 999px;
		font-size: 12px;
		font-weight: 500;
		white-space: nowrap;
		text-transform: capitalize;
		background: var(--neutral-soft);
		color: var(--text-2);
	}
	.dot {
		width: 6px;
		height: 6px;
		border-radius: 50%;
		background: var(--neutral);
		flex: none;
	}
	.ok {
		background: var(--ok-soft);
		color: var(--ok);
	}
	.ok .dot {
		background: var(--ok);
	}
	.warn {
		background: var(--warn-soft);
		color: var(--warn);
	}
	.warn .dot {
		background: var(--warn);
	}
	.bad {
		background: var(--bad-soft);
		color: var(--bad);
	}
	.bad .dot {
		background: var(--bad);
	}
	.info {
		background: var(--info-soft);
		color: var(--info);
	}
	.info .dot {
		background: var(--info);
	}
	.pulse .dot {
		box-shadow: 0 0 0 0 currentColor;
		animation: pulse 2s infinite;
	}
	@keyframes pulse {
		0% {
			box-shadow: 0 0 0 0 color-mix(in srgb, currentColor 45%, transparent);
		}
		70% {
			box-shadow: 0 0 0 5px transparent;
		}
		100% {
			box-shadow: 0 0 0 0 transparent;
		}
	}
</style>
