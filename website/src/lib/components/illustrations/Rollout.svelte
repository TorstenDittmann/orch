<!-- A rolling update: each replica is replaced in turn and only counts once its
     health check passes. -->
<svg class="illo" viewBox="0 0 440 232" role="img" aria-label="Three replicas are replaced one at a time, each passing a health check">
	{#each [1, 2, 3] as n}
		<g class="row-{n}" transform="translate(0 {(n - 1) * 80})">
			<rect class="box" x="0.5" y="0.5" width="439" height="71" rx="10" />
			<text class="strong" x="18" y="40">web-{n}</text>
			<rect class="pill" x="100.5" y="20.5" width="100" height="30" rx="6" />
			<text x="150.5" y="40" text-anchor="middle">api:1</text>
			<g class="next scale">
				<rect class="on" x="100.5" y="20.5" width="100" height="30" rx="6" />
				<text x="150.5" y="40" text-anchor="middle">api:2</text>
			</g>
			<g class="checking">
				<circle class="spinner scale" cx="300" cy="36" r="6" />
				<text x="316" y="40">checking</text>
			</g>
			<g class="healthy">
				<circle class="dot" cx="300" cy="36" r="5" />
				<text x="316" y="40">healthy</text>
			</g>
		</g>
	{/each}
</svg>

<style>
	svg {
		--loop: 8s;
	}

	.spinner {
		fill: none;
		stroke: var(--accent);
		stroke-width: 2;
		stroke-dasharray: 26 12;
		animation: spin 0.9s linear infinite;
	}

	.next,
	.checking,
	.healthy {
		animation-duration: var(--loop);
		animation-iteration-count: infinite;
		animation-fill-mode: both;
		animation-timing-function: ease-out;
	}

	.checking {
		opacity: 0;
	}

	.row-1 .next {
		animation-name: next-1;
	}

	.row-1 .checking {
		animation-name: checking-1;
	}

	.row-1 .healthy {
		animation-name: healthy-1;
	}

	.row-2 .next {
		animation-name: next-2;
	}

	.row-2 .checking {
		animation-name: checking-2;
	}

	.row-2 .healthy {
		animation-name: healthy-2;
	}

	.row-3 .next {
		animation-name: next-3;
	}

	.row-3 .checking {
		animation-name: checking-3;
	}

	.row-3 .healthy {
		animation-name: healthy-3;
	}

	@keyframes spin {
		to {
			transform: rotate(1turn);
		}
	}

	@keyframes next-1 {
		0%,
		10% {
			opacity: 0;
			transform: scale(0.85);
		}
		14%,
		92% {
			opacity: 1;
			transform: none;
		}
		98%,
		100% {
			opacity: 0;
			transform: none;
		}
	}

	@keyframes checking-1 {
		0%,
		10%,
		24%,
		100% {
			opacity: 0;
		}
		12%,
		22% {
			opacity: 1;
		}
	}

	@keyframes healthy-1 {
		0%,
		10%,
		24%,
		100% {
			opacity: 1;
		}
		12%,
		22% {
			opacity: 0;
		}
	}

	@keyframes next-2 {
		0%,
		34% {
			opacity: 0;
			transform: scale(0.85);
		}
		38%,
		92% {
			opacity: 1;
			transform: none;
		}
		98%,
		100% {
			opacity: 0;
			transform: none;
		}
	}

	@keyframes checking-2 {
		0%,
		34%,
		48%,
		100% {
			opacity: 0;
		}
		36%,
		46% {
			opacity: 1;
		}
	}

	@keyframes healthy-2 {
		0%,
		34%,
		48%,
		100% {
			opacity: 1;
		}
		36%,
		46% {
			opacity: 0;
		}
	}

	@keyframes next-3 {
		0%,
		58% {
			opacity: 0;
			transform: scale(0.85);
		}
		62%,
		92% {
			opacity: 1;
			transform: none;
		}
		98%,
		100% {
			opacity: 0;
			transform: none;
		}
	}

	@keyframes checking-3 {
		0%,
		58%,
		72%,
		100% {
			opacity: 0;
		}
		60%,
		70% {
			opacity: 1;
		}
	}

	@keyframes healthy-3 {
		0%,
		58%,
		72%,
		100% {
			opacity: 1;
		}
		60%,
		70% {
			opacity: 0;
		}
	}
</style>
