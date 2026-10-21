<!-- An AI agent works the cluster through orch's MCP server: it reads status
     and logs, then deploys a fix with a token scoped to one application. -->
<svg class="illo" viewBox="0 0 440 300" role="img" aria-label="An AI assistant checks status and logs through orch, then deploys a fix">
	<g class="step-0">
		<rect class="pill" x="210.5" y="0.5" width="229" height="36" rx="18" />
		<text class="strong" x="226" y="23">why is shop not ready?</text>
	</g>
	<g class="step-1">
		<rect class="box" x="0.5" y="56.5" width="329" height="34" rx="8" />
		<circle class="dot" cx="18" cy="73" r="4" />
		<text x="32" y="77"><tspan class="strong">app_status</tspan> shop</text>
		<rect class="pill" x="258.5" y="63.5" width="58" height="20" rx="10" />
		<text x="287.5" y="77" text-anchor="middle">read</text>
	</g>
	<g class="step-2">
		<rect class="box" x="0.5" y="100.5" width="329" height="34" rx="8" />
		<circle class="dot" cx="18" cy="117" r="4" />
		<text x="32" y="121"><tspan class="strong">logs</tspan> shop</text>
		<rect class="pill" x="258.5" y="107.5" width="58" height="20" rx="10" />
		<text x="287.5" y="121" text-anchor="middle">read</text>
	</g>
	<g class="step-3">
		<rect class="box" x="0.5" y="144.5" width="329" height="34" rx="8" />
		<circle class="dot" cx="18" cy="161" r="4" />
		<text x="32" y="165"><tspan class="strong">deploy</tspan> orch.pkl</text>
		<rect class="on" x="258.5" y="151.5" width="58" height="20" rx="10" />
		<text x="287.5" y="165" text-anchor="middle">write</text>
	</g>
	<g class="step-4">
		<rect class="on" x="0.5" y="194.5" width="329" height="56" rx="12" />
		<text x="16" y="218">Set the missing DATABASE_URL</text>
		<text x="16" y="238" style="fill: var(--accent)">and redeployed. shop is healthy.</text>
	</g>
	<text x="0" y="286">ORCH_TOKEN · scoped to shop</text>
</svg>

<style>
	g {
		animation: 8s ease-out infinite both;
	}

	.step-0 {
		animation-name: step-0;
	}

	.step-1 {
		animation-name: step-1;
	}

	.step-2 {
		animation-name: step-2;
	}

	.step-3 {
		animation-name: step-3;
	}

	.step-4 {
		animation-name: step-4;
	}

	@keyframes step-0 {
		0%,
		2% {
			opacity: 0;
			transform: translateY(6px);
		}
		6%,
		90% {
			opacity: 1;
			transform: none;
		}
		97%,
		100% {
			opacity: 0;
			transform: none;
		}
	}

	@keyframes step-1 {
		0%,
		14% {
			opacity: 0;
			transform: translateY(6px);
		}
		18%,
		90% {
			opacity: 1;
			transform: none;
		}
		97%,
		100% {
			opacity: 0;
			transform: none;
		}
	}

	@keyframes step-2 {
		0%,
		26% {
			opacity: 0;
			transform: translateY(6px);
		}
		30%,
		90% {
			opacity: 1;
			transform: none;
		}
		97%,
		100% {
			opacity: 0;
			transform: none;
		}
	}

	@keyframes step-3 {
		0%,
		38% {
			opacity: 0;
			transform: translateY(6px);
		}
		42%,
		90% {
			opacity: 1;
			transform: none;
		}
		97%,
		100% {
			opacity: 0;
			transform: none;
		}
	}

	@keyframes step-4 {
		0%,
		54% {
			opacity: 0;
			transform: translateY(6px);
		}
		58%,
		90% {
			opacity: 1;
			transform: none;
		}
		97%,
		100% {
			opacity: 0;
			transform: none;
		}
	}
</style>
