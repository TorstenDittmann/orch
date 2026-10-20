<!-- One application file is deployed across the cluster: a request leaves the
     file, fans out to each node, and a replica starts and turns healthy. -->
<svg class="illo" viewBox="0 0 440 330" role="img" aria-label="One application file deployed to three nodes, each starting a healthy replica">
	<g>
		<rect class="box" x="0.5" y="60.5" width="140" height="170" rx="10" />
		<text x="16" y="86">api.pkl</text>
		<line class="wire" x1="0.5" x2="140.5" y1="100.5" y2="100.5" />
		<text x="16" y="128"><tspan class="strong">name</tspan> = <tspan class="on">"api"</tspan></text>
		<text x="16" y="152"><tspan class="strong">image</tspan> = <tspan class="on">"api:2"</tspan></text>
		<text x="16" y="176"><tspan class="strong">replicas</tspan> = <tspan class="on">3</tspan></text>
		<text x="16" y="200"><tspan class="strong">port</tspan> = <tspan class="on">8080</tspan></text>
		<text x="0" y="262" class="strong">$ orch deploy</text>
	</g>

	{#each [53, 165, 277] as y, i}
		<line class="wire" x1="140" y1="145" x2="220" y2={y} stroke-dasharray="3 4" />
		<circle class="dot packet" cx="140" cy="145" r="4" style="--dx: 80px; --dy: {y - 145}px; animation-delay: {i * 0.12}s" />

		<g transform="translate(220 {y - 43})">
			<rect class="box" x="0.5" y="0.5" width="219" height="86" rx="10" />
			<text x="16" y="26">node-{i + 1}</text>
			<circle cx="200" cy="22" r="4" fill="var(--edge)" />
			<circle class="dot healthy" cx="200" cy="22" r="4" style="animation-delay: {0.3 + i * 0.25}s" />
			<g class="replica scale" style="animation-delay: {i * 0.25}s">
				<rect class="on" x="16.5" y="40.5" width="88" height="30" rx="6" />
				<text x="60" y="60" text-anchor="middle">api</text>
			</g>
			<rect class="pill" x="114.5" y="40.5" width="89" height="30" rx="6" />
			<text x="159" y="60" text-anchor="middle">{["db", "worker", "cron"][i]}</text>
		</g>
	{/each}
</svg>

<style>
	svg {
		--loop: 6s;
	}

	.packet {
		opacity: 0;
		animation: packet var(--loop) ease-in-out infinite both;
	}

	.replica {
		animation: replica var(--loop) ease-out infinite both;
	}

	.healthy {
		animation: healthy var(--loop) ease-out infinite both;
	}

	@keyframes packet {
		0%,
		8% {
			opacity: 0;
			transform: none;
		}
		10% {
			opacity: 1;
		}
		26% {
			opacity: 1;
			transform: translate(var(--dx), var(--dy));
		}
		28%,
		100% {
			opacity: 0;
			transform: translate(var(--dx), var(--dy));
		}
	}

	@keyframes replica {
		0%,
		26% {
			opacity: 0;
			transform: scale(0.6);
		}
		32%,
		88% {
			opacity: 1;
			transform: none;
		}
		96%,
		100% {
			opacity: 0;
			transform: none;
		}
	}

	@keyframes healthy {
		0%,
		38% {
			opacity: 0;
		}
		42%,
		88% {
			opacity: 1;
		}
		96%,
		100% {
			opacity: 0;
		}
	}
</style>
