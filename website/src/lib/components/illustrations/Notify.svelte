<!-- A failed deploy is reported to every configured target at once. -->
<svg class="illo" viewBox="0 0 440 250" role="img" aria-label="A failed deploy sends a message to Slack, Discord and a webhook">
	<rect class="box" x="0.5" y="88.5" width="149" height="72" rx="10" />
	<text class="strong" x="75" y="118" text-anchor="middle">shop</text>
	<text class="bad" x="75" y="142" text-anchor="middle">deploy failed</text>

	{#each ["Slack", "Discord", "Webhook"] as name, i}
		{@const y = 10 + i * 82}
		<line class="wire" x1="150" y1="124.5" x2="290" y2={y + 32.5} stroke-dasharray="3 4" />
		<circle class="dot packet" cx="150" cy="124.5" r="4" style="--dx: 140px; --dy: {y - 92}px; animation-delay: {i * 0.12}s" />
		<rect class="box" x="290.5" y={y + 0.5} width="149" height="64" rx="10" />
		<text class="strong" x="306" y={y + 24}>{name}</text>
		<g class="message" style="animation-delay: {i * 0.12}s">
			<rect class="bad-bar" x="306" y={y + 36} width="8" height="12" rx="2" />
			<rect class="bar" x="320" y={y + 36} width={[96, 80, 104][i]} height="12" rx="3" />
		</g>
	{/each}
</svg>

<style>
	svg {
		--loop: 6s;
		--bad: #d4473f;
	}

	.bad {
		fill: var(--bad);
		animation: event var(--loop) ease-out infinite both;
	}

	.bad-bar {
		fill: var(--bad);
	}

	.bar {
		fill: var(--edge);
	}

	.packet {
		opacity: 0;
		animation: packet var(--loop) ease-in-out infinite both;
	}

	.message {
		animation: message var(--loop) ease-out infinite both;
	}

	@keyframes event {
		0%,
		6% {
			opacity: 0;
		}
		10%,
		90% {
			opacity: 1;
		}
		97%,
		100% {
			opacity: 0;
		}
	}

	@keyframes packet {
		0%,
		12% {
			opacity: 0;
			transform: none;
		}
		14% {
			opacity: 1;
		}
		30% {
			opacity: 1;
			transform: translate(var(--dx), var(--dy));
		}
		32%,
		100% {
			opacity: 0;
			transform: translate(var(--dx), var(--dy));
		}
	}

	@keyframes message {
		0%,
		30% {
			opacity: 0;
			transform: translateY(4px);
		}
		36%,
		90% {
			opacity: 1;
			transform: none;
		}
		97%,
		100% {
			opacity: 0;
		}
	}
</style>
