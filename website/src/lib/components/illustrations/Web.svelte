<!-- `orch web` opens a dashboard on loopback: the overview of applications,
     with a deploy finishing live as billing-api turns from starting to running. -->
<svg class="illo" viewBox="0 0 440 300" role="img" aria-label="The orch web dashboard in a browser, showing applications and nodes while a deploy finishes">
	<rect class="box" x="0.5" y="0.5" width="439" height="299" rx="10" />
	<circle class="light" cx="16" cy="16" r="3.5" />
	<circle class="light" cx="28" cy="16" r="3.5" />
	<circle class="light" cx="40" cy="16" r="3.5" />
	<rect class="pill" x="60.5" y="7.5" width="230" height="17" rx="8.5" />
	<text class="small" x="72" y="20">127.0.0.1:49731 · orch web</text>
	<line class="wire" x1="0" y1="32.5" x2="440" y2="32.5" />

	<!-- sidebar -->
	<line class="wire" x1="110.5" y1="33" x2="110.5" y2="299" />
	<rect class="on" x="8.5" y="44.5" width="94" height="20" rx="5" />
	<text class="small" x="18" y="58">Overview</text>
	<text class="small" x="18" y="84">Applications</text>
	<text class="small" x="18" y="106">Runtimes</text>
	<text class="small" x="18" y="128">Nodes</text>
	<text class="small" x="18" y="150">Jobs</text>
	<text class="small" x="18" y="172">Events</text>
	<circle class="dot" cx="21" cy="280" r="3" />
	<text class="small" x="30" y="284">Live</text>

	<!-- header and stat cards -->
	<text class="strong title" x="126" y="58">production</text>
	<text class="small" x="126" y="74">3 nodes · ssh</text>

	<rect class="pill" x="126.5" y="86.5" width="91" height="48" rx="7" />
	<text class="small" x="136" y="102">Apps</text>
	<text class="strong value before" x="136" y="124">3/4</text>
	<text class="strong value after" x="136" y="124">4/4</text>

	<rect class="pill" x="229.5" y="86.5" width="91" height="48" rx="7" />
	<text class="small" x="239" y="102">Nodes</text>
	<text class="strong value" x="239" y="124">3/3</text>

	<rect class="pill" x="332.5" y="86.5" width="91" height="48" rx="7" />
	<text class="small" x="342" y="102">CPU reserved</text>
	<text class="strong value" x="342" y="124">61%</text>

	<!-- applications -->
	<rect class="pill" x="126.5" y="146.5" width="297" height="136" rx="7" />
	<text class="strong" x="138" y="166">Applications</text>
	<line class="wire" x1="127" y1="176.5" x2="423" y2="176.5" />

	<text class="strong" x="138" y="198">storefront</text>
	<text class="small" x="230" y="198">gen 18</text>
	<rect class="pill" x="342.5" y="186.5" width="70" height="16" rx="8" />
	<circle class="dot" cx="353" cy="194.5" r="2.5" />
	<text class="small" x="360" y="198">running</text>

	<text class="strong" x="138" y="222">billing-api</text>
	<text class="small" x="230" y="222">gen 7</text>
	<g class="before">
		<rect class="pill" x="342.5" y="210.5" width="70" height="16" rx="8" />
		<circle class="wire" cx="353" cy="218.5" r="2.5" />
		<text class="small" x="360" y="222">starting</text>
	</g>
	<g class="after">
		<rect class="on" x="342.5" y="210.5" width="70" height="16" rx="8" />
		<circle class="dot" cx="353" cy="218.5" r="2.5" />
		<text class="small" x="360" y="222">running</text>
	</g>

	<text class="strong" x="138" y="246">reports</text>
	<text class="small" x="230" y="246">gen 3</text>
	<rect class="pill" x="342.5" y="234.5" width="70" height="16" rx="8" />
	<circle class="dot" cx="353" cy="242.5" r="2.5" />
	<text class="small" x="360" y="246">running</text>

	<text class="strong" x="138" y="270">postgres</text>
	<text class="small" x="230" y="270">gen 2</text>
	<rect class="pill" x="342.5" y="258.5" width="70" height="16" rx="8" />
	<circle class="dot" cx="353" cy="266.5" r="2.5" />
	<text class="small" x="360" y="270">running</text>
</svg>

<style>
	.light {
		fill: var(--edge);
	}

	.small {
		font-size: 11px;
	}

	.title {
		font-size: 14px;
	}

	.value {
		font-size: 16px;
	}

	/* The final frame is the default, so reduced motion shows the finished
	   deploy: the "before" state is hidden unless it is animating. */
	.before {
		opacity: 0;
		animation: before 6s ease-in-out infinite both;
	}

	.after {
		animation: after 6s ease-in-out infinite both;
	}

	@keyframes before {
		0%,
		45% {
			opacity: 1;
		}
		52%,
		100% {
			opacity: 0;
		}
	}

	@keyframes after {
		0%,
		45% {
			opacity: 0;
		}
		52%,
		100% {
			opacity: 1;
		}
	}
</style>
