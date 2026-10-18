<script module lang="ts">
	export interface Point {
		time: number;
		value: number;
	}
</script>

<script lang="ts">
	let {
		label,
		points,
		total,
		stepSeconds = 300,
		now,
		format,
		headline,
		detail = ''
	}: {
		label: string;
		points: Point[];
		total: number;
		stepSeconds?: number;
		now: number;
		format: (value: number) => string;
		headline: string;
		detail?: string;
	} = $props();

	const height = 112;
	const pad = { top: 6, bottom: 20 };
	const span = 24 * 3600 * 1000;

	let width = $state(0);
	let hover = $state<Point | null>(null);

	const start = $derived(now - span);
	const plot = $derived(height - pad.top - pad.bottom);
	const visible = $derived(points.filter((point) => point.time >= start - stepSeconds * 1000 && point.time <= now + stepSeconds * 1000));

	function x(time: number) {
		return Math.max(0, Math.min(width, ((time - start) / span) * width));
	}
	function y(value: number) {
		const fraction = total > 0 ? Math.max(0, Math.min(1, value / total)) : 0;
		return pad.top + plot * (1 - fraction);
	}

	// Consecutive samples further apart than ~1.5 buckets break the line.
	const segments = $derived.by(() => {
		const result: Point[][] = [];
		let current: Point[] = [];
		for (const point of visible) {
			const last = current[current.length - 1];
			if (last && point.time - last.time > stepSeconds * 1500) {
				result.push(current);
				current = [];
			}
			current.push(point);
		}
		if (current.length) result.push(current);
		return result;
	});

	const paths = $derived(
		width > 0
			? segments.map((segment) => {
					const line = segment.map((point, index) => `${index ? 'L' : 'M'}${x(point.time).toFixed(1)},${y(point.value).toFixed(1)}`).join('');
					const base = y(0).toFixed(1);
					const area = `${line}L${x(segment[segment.length - 1].time).toFixed(1)},${base}L${x(segment[0].time).toFixed(1)},${base}Z`;
					return { line, area, single: segment.length === 1 ? segment[0] : null };
				})
			: []
	);

	const ticks = [
		{ offset: 24, text: '24h ago' },
		{ offset: 18, text: '18h' },
		{ offset: 12, text: '12h' },
		{ offset: 6, text: '6h' },
		{ offset: 0, text: 'now' }
	];

	function onpointermove(event: PointerEvent) {
		if (!visible.length || width <= 0) return;
		const rect = (event.currentTarget as SVGElement).getBoundingClientRect();
		const time = start + ((event.clientX - rect.left) / rect.width) * span;
		let best = visible[0];
		for (const point of visible) if (Math.abs(point.time - time) < Math.abs(best.time - time)) best = point;
		hover = Math.abs(best.time - time) <= stepSeconds * 1500 ? best : null;
	}

	function clock(time: number) {
		return new Date(time).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
	}

	const percent = (value: number) => (total > 0 ? `${Math.round((value / total) * 100)}%` : '');
	const summary = $derived(`${label} over the last 24 hours, ${visible.length} samples, current ${headline}`);
</script>

<div class="usage">
	<div class="head">
		<span class="label">{label}</span>
		<span class="value">{headline}</span>
		{#if detail}<span class="detail">{detail}</span>{/if}
	</div>
	<div class="plot" bind:clientWidth={width}>
		{#if width > 0}
			<svg {width} {height} role="img" aria-label={summary} onpointermove={onpointermove} onpointerleave={() => (hover = null)}>
				<line class="grid" x1="0" x2={width} y1={y(total)} y2={y(total)} />
				<line class="grid" x1="0" x2={width} y1={y(total / 2)} y2={y(total / 2)} />
				<line class="axis" x1="0" x2={width} y1={y(0)} y2={y(0)} />
				{#each paths as path, index (index)}
					{#if path.single}
						<circle class="dot" cx={x(path.single.time)} cy={y(path.single.value)} r="2" />
					{:else}
						<path class="area" d={path.area} />
						<path class="line" d={path.line} />
					{/if}
				{/each}
				{#each ticks as tick (tick.offset)}
					<text class="tick" x={x(now - tick.offset * 3600 * 1000)} y={height - 5} text-anchor={tick.offset === 24 ? 'start' : tick.offset === 0 ? 'end' : 'middle'}>{tick.text}</text>
				{/each}
				{#if hover}
					<line class="cross" x1={x(hover.time)} x2={x(hover.time)} y1={pad.top} y2={y(0)} />
					<circle class="hover-dot" cx={x(hover.time)} cy={y(hover.value)} r="4" />
				{/if}
				<rect class="hit" x="0" y="0" {width} {height} />
			</svg>
			{#if hover}
				{@const left = x(hover.time)}
				<div class="tip" style:left="{left}px" class:flip={left > width - 130}>
					<span class="faint">{clock(hover.time)}</span>
					<strong>{format(hover.value)}</strong>
					<span class="faint">{percent(hover.value)}</span>
				</div>
			{/if}
			{#if !visible.length}
				<div class="none">No samples in the last 24 hours</div>
			{/if}
		{/if}
	</div>
</div>

<style>
	.usage {
		min-width: 0;
	}
	.head {
		display: flex;
		align-items: baseline;
		gap: 8px;
		flex-wrap: wrap;
		margin-bottom: 10px;
	}
	.label {
		color: var(--text-2);
		font-size: 13px;
		font-weight: 500;
		width: 100%;
	}
	.value {
		font-size: 22px;
		font-weight: 650;
		letter-spacing: -0.02em;
		line-height: 1.15;
		font-variant-numeric: tabular-nums;
	}
	.detail {
		color: var(--text-3);
		font-size: 12.5px;
	}
	.plot {
		position: relative;
		height: 112px;
	}
	svg {
		display: block;
		overflow: visible;
	}
	.grid {
		stroke: var(--border);
		stroke-dasharray: 3 3;
	}
	.axis {
		stroke: var(--border-strong);
	}
	.area {
		fill: var(--accent);
		fill-opacity: 0.14;
	}
	.line {
		fill: none;
		stroke: var(--accent);
		stroke-width: 1.75;
		stroke-linejoin: round;
		stroke-linecap: round;
	}
	.dot {
		fill: var(--accent);
	}
	.tick {
		fill: var(--text-3);
		font-size: 11px;
	}
	.cross {
		stroke: var(--text-3);
		stroke-width: 1;
	}
	.hover-dot {
		fill: var(--accent);
		stroke: var(--surface);
		stroke-width: 2;
	}
	.hit {
		fill: transparent;
	}
	.tip {
		position: absolute;
		top: -6px;
		transform: translateX(8px);
		display: flex;
		gap: 6px;
		align-items: baseline;
		padding: 4px 8px;
		background: var(--surface);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		box-shadow: var(--shadow);
		font-size: 12px;
		white-space: nowrap;
		pointer-events: none;
		font-variant-numeric: tabular-nums;
	}
	.tip.flip {
		transform: translateX(calc(-100% - 8px));
	}
	.none {
		position: absolute;
		inset: 0 0 20px;
		display: grid;
		place-items: center;
		color: var(--text-3);
		font-size: 12.5px;
	}
</style>
