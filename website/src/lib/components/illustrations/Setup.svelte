<!-- Building a cluster: init a controller over SSH, then workers join and link
     up over encrypted tunnels. -->
<svg class="illo" viewBox="0 0 440 290" role="img" aria-label="A laptop reaches the controller over SSH, and three workers join it">
	<g class="laptop">
		<rect class="pill" x="0.5" y="26.5" width="100" height="44" rx="8" />
		<text x="50" y="53" text-anchor="middle">laptop</text>
	</g>
	<line class="wire link ssh" x1="101" y1="48.5" x2="140" y2="48.5" pathLength="1" />
	<text class="ssh-label" x="120" y="38" text-anchor="middle">ssh</text>

	<g class="controller">
		<rect class="box" x="140.5" y="10.5" width="159" height="76" rx="10" />
		<text class="strong" x="220" y="42" text-anchor="middle">controller</text>
		<text x="220" y="66" text-anchor="middle" style="fill: var(--accent)">orch init</text>
	</g>

	{#each [70, 220, 370] as x, i}
		<line class="wire link join" x1="220" y1="87" x2={x} y2="200" pathLength="1" style="animation-delay: {i * 0.25}s" />
		<g class="lock scale" style="animation-delay: {i * 0.25}s">
			<circle cx={(220 + x) / 2} cy="143.5" r="12" fill="var(--bg)" stroke="var(--accent)" />
			<rect x={(220 + x) / 2 - 5} y="142" width="10" height="7" rx="1.5" fill="var(--accent)" />
			<path d="M{(220 + x) / 2 - 3} 142v-2.5a3 3 0 0 1 6 0v2.5" fill="none" stroke="var(--accent)" stroke-width="1.5" />
		</g>
		<g class="worker" style="animation-delay: {i * 0.25}s">
			<rect class="box" x={x - 69.5} y="200.5" width="139" height="76" rx="10" />
			<text class="strong" x={x} y="232" text-anchor="middle">worker-{i + 1}</text>
			<text x={x} y="256" text-anchor="middle" style="fill: var(--accent)">orch join</text>
		</g>
	{/each}
</svg>

<style>
	svg {
		--loop: 7s;
	}

	.controller,
	.laptop {
		animation: appear var(--loop) ease-out infinite both;
	}

	.link {
		stroke-dasharray: 1;
	}

	.ssh,
	.ssh-label {
		animation: ssh var(--loop) ease-in-out infinite both;
	}

	.join {
		stroke: var(--accent);
		animation: draw var(--loop) ease-in-out infinite both;
	}

	.lock {
		animation: lock var(--loop) ease-out infinite both;
	}

	.worker {
		animation: join var(--loop) ease-out infinite both;
	}

	@keyframes appear {
		0% {
			opacity: 0;
		}
		6%,
		90% {
			opacity: 1;
		}
		98%,
		100% {
			opacity: 0;
		}
	}

	@keyframes ssh {
		0%,
		6% {
			stroke-dashoffset: 1;
			opacity: 0;
		}
		8% {
			opacity: 1;
		}
		14%,
		90% {
			stroke-dashoffset: 0;
			opacity: 1;
		}
		98%,
		100% {
			stroke-dashoffset: 0;
			opacity: 0;
		}
	}

	@keyframes draw {
		0%,
		18% {
			stroke-dashoffset: 1;
		}
		32%,
		90% {
			stroke-dashoffset: 0;
			opacity: 1;
		}
		98%,
		100% {
			stroke-dashoffset: 0;
			opacity: 0;
		}
	}

	@keyframes lock {
		0%,
		32% {
			opacity: 0;
			transform: scale(0.4);
		}
		37%,
		90% {
			opacity: 1;
			transform: none;
		}
		98%,
		100% {
			opacity: 0;
			transform: none;
		}
	}

	@keyframes join {
		0%,
		30% {
			opacity: 0.25;
		}
		36%,
		90% {
			opacity: 1;
		}
		98%,
		100% {
			opacity: 0.25;
		}
	}
</style>
