<!-- A database declared in the manifest: orch starts it, hands the service a
     generated connection URL, and takes scheduled backups. -->
<svg class="illo" viewBox="0 0 440 250" role="img" aria-label="A Postgres database starts, passes a generated connection URL to the web service, and is backed up nightly">
	<rect class="pill" x="0.5" y="0.5" width="439" height="36" rx="8" />
	<text x="16" y="23"><tspan class="strong">databases</tspan> {"{"} ["main"] = new <tspan class="on">Postgres</tspan> {"{}"} {"}"}</text>

	<rect class="box" x="0.5" y="80.5" width="139" height="76" rx="10" />
	<text class="strong" x="70" y="112" text-anchor="middle">web</text>
	<text class="url" x="70" y="136" text-anchor="middle">DATABASE_URL ✓</text>

	<line class="wire" x1="140" y1="118.5" x2="300" y2="118.5" stroke-dasharray="3 4" />
	<g class="secret">
		<rect class="on" x="186.5" y="104.5" width="68" height="28" rx="14" />
		<text x="220.5" y="123" text-anchor="middle">main-url</text>
	</g>

	<g class="db scale">
		<path class="box" d="M300.5 68.5v100c0 9 31 16 69.5 16s69.5-7 69.5-16v-100" />
		<ellipse class="box" cx="370" cy="68.5" rx="69.5" ry="16" />
		<text class="strong" x="370" y="118" text-anchor="middle">main</text>
		<text x="370" y="140" text-anchor="middle">postgres 17</text>
	</g>

	<text x="300" y="222">backups</text>
	{#each [0, 1, 2] as i}
		<rect class="on backup scale" x={364.5 + i * 26} y="208.5" width="18" height="18" rx="4" style="animation-delay: {i * 0.35}s" />
	{/each}
</svg>

<style>
	svg {
		--loop: 7s;
	}

	.db {
		animation: db var(--loop) ease-out infinite both;
	}

	.secret {
		animation: secret var(--loop) ease-in-out infinite both;
	}

	.url {
		fill: var(--accent);
		animation: url var(--loop) ease-out infinite both;
	}

	.backup {
		animation: backup var(--loop) ease-out infinite both;
	}

	@keyframes db {
		0%,
		4% {
			opacity: 0;
			transform: scale(0.85);
		}
		12%,
		92% {
			opacity: 1;
			transform: none;
		}
		98%,
		100% {
			opacity: 0;
		}
	}

	/* the generated URL travels from the database to the service */
	@keyframes secret {
		0%,
		16% {
			opacity: 0;
			transform: translateX(90px);
		}
		20% {
			opacity: 1;
			transform: translateX(90px);
		}
		36% {
			opacity: 1;
			transform: translateX(-40px);
		}
		40%,
		100% {
			opacity: 0;
			transform: translateX(-40px);
		}
	}

	@keyframes url {
		0%,
		36% {
			opacity: 0;
		}
		40%,
		92% {
			opacity: 1;
		}
		98%,
		100% {
			opacity: 0;
		}
	}

	@keyframes backup {
		0%,
		50% {
			opacity: 0;
			transform: scale(0.5);
		}
		55%,
		92% {
			opacity: 1;
			transform: none;
		}
		98%,
		100% {
			opacity: 0;
		}
	}
</style>
