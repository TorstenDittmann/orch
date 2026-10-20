<!-- A request arrives over HTTPS at the ingress node and crosses the encrypted
     mesh to the node running the service, then the response goes back. -->
<svg class="illo" viewBox="0 0 440 280" role="img" aria-label="An HTTPS request reaches the ingress node and travels over the WireGuard mesh to the service">
	<rect class="pill" x="100.5" y="0.5" width="239" height="36" rx="18" />
	<g fill="var(--accent)">
		<rect x="119" y="17" width="10" height="7" rx="1.5" />
		<path d="M121 17v-2.5a3 3 0 0 1 6 0v2.5" fill="none" stroke="var(--accent)" stroke-width="1.5" />
	</g>
	<text class="strong" x="138" y="23">https://api.example.com</text>

	<line class="wire" x1="220" y1="37" x2="220" y2="100" />
	<line class="tunnel" x1="190" y1="165" x2="70" y2="214" />
	<line class="tunnel" x1="250" y1="165" x2="370" y2="214" />
	<line class="tunnel" x1="140" y1="246" x2="300" y2="246" />
	<text x="220" y="236" text-anchor="middle">wireguard</text>

	<rect class="box" x="140.5" y="100.5" width="159" height="64" rx="10" />
	<text class="strong" x="220" y="128" text-anchor="middle">node-1</text>
	<text x="220" y="150" text-anchor="middle" style="fill: var(--accent)">ingress · tls</text>

	<rect class="box" x="0.5" y="214.5" width="139" height="64" rx="10" />
	<text class="strong" x="70" y="242" text-anchor="middle">node-2</text>
	<text x="70" y="264" text-anchor="middle">db</text>

	<rect class="box" x="300.5" y="214.5" width="139" height="64" rx="10" />
	<text class="strong" x="370" y="242" text-anchor="middle">node-3</text>
	<text x="370" y="264" text-anchor="middle" style="fill: var(--accent)">api</text>

	<!-- request down and across, response back the same way -->
	<circle class="dot packet" cx="220" cy="37" r="4" style="--dx: 0px; --dy: 63px" />
	<circle class="dot packet" cx="250" cy="165" r="4" style="--dx: 120px; --dy: 49px; animation-delay: 1.4s" />
	<circle class="dot packet back" cx="370" cy="214" r="4" style="--dx: -120px; --dy: -49px; animation-delay: 3.6s" />
	<circle class="dot packet back" cx="220" cy="100" r="4" style="--dx: 0px; --dy: -63px; animation-delay: 5s" />
</svg>

<style>
	.tunnel {
		stroke: var(--accent);
		stroke-width: 1.5;
		stroke-dasharray: 4 5;
		animation: flow 1.2s linear infinite;
	}

	.packet {
		opacity: 0;
		animation: hop 7s ease-in-out infinite both;
	}

	.back {
		fill: var(--fg);
	}

	@keyframes flow {
		to {
			stroke-dashoffset: -18;
		}
	}

	@keyframes hop {
		0% {
			opacity: 0;
			transform: none;
		}
		2% {
			opacity: 1;
		}
		18% {
			opacity: 1;
			transform: translate(var(--dx), var(--dy));
		}
		20%,
		100% {
			opacity: 0;
			transform: translate(var(--dx), var(--dy));
		}
	}
</style>
