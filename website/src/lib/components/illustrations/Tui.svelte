<!-- `orch tui` in a terminal: the header, view tabs and overview cards of the
     real interface, with cluster events streaming into the activity list. -->
<svg class="illo" viewBox="0 0 440 300" role="img" aria-label="The orch terminal UI showing the cluster overview while new events stream in">
	<rect class="box" x="0.5" y="0.5" width="439" height="299" rx="10" />
	<circle class="light" cx="16" cy="16" r="3.5" />
	<circle class="light" cx="28" cy="16" r="3.5" />
	<circle class="light" cx="40" cy="16" r="3.5" />
	<text class="t" x="220" y="20" text-anchor="middle">orch tui</text>
	<line class="wire" x1="0" y1="32.5" x2="440" y2="32.5" />

	<!-- header bar -->
	<rect class="bar" x="8" y="40" width="424" height="18" />
	<text class="t strong b" x="14" y="53">ORCH</text>
	<text class="t strong" x="52" y="53">production</text>
	<text class="t on-text" x="306" y="53" text-anchor="end">● ONLINE</text>
	<text class="t" x="426" y="53" text-anchor="end">production · ssh</text>

	<!-- tabs -->
	<rect class="on" x="10.5" y="64.5" width="80" height="16" />
	<text class="t" x="16" y="76">1 Overview</text>
	<text class="t" x="102" y="76">2 Applications</text>
	<text class="t" x="206" y="76">3 Nodes</text>
	<text class="t" x="266" y="76">4 Events</text>
	<text class="t" x="332" y="76">5 Runtimes</text>

	<!-- metric cards -->
	<rect class="pill" x="8.5" y="90.5" width="134" height="46" />
	<text class="t" x="18" y="107">APPLICATIONS</text>
	<text class="t strong" x="18" y="126">4</text>
	<text class="t" x="35" y="126">healthy</text>
	<rect class="pill" x="152.5" y="90.5" width="134" height="46" />
	<text class="t" x="162" y="107">NODES</text>
	<text class="t strong" x="162" y="126">3</text>
	<text class="t" x="179" y="126">connected</text>
	<rect class="pill" x="296.5" y="90.5" width="135" height="46" />
	<text class="t" x="306" y="107">EVENT CURSOR</text>
	<text class="t strong" x="306" y="126">1207</text>
	<text class="t" x="344" y="126">retained</text>

	<!-- recent activity -->
	<text class="t strong" x="14" y="160">Recent activity</text>
	<g class="ev ev-0">
		<text class="t" x="14" y="182">12:04:31</text>
		<text class="t strong" x="80" y="182">deployment.started</text>
		<text class="t" x="240" y="182">billing-api gen 7</text>
	</g>
	<g class="ev ev-1">
		<text class="t" x="14" y="200">12:04:36</text>
		<text class="t strong" x="80" y="200">allocation.ready</text>
		<text class="t" x="240" y="200">billing-api/api#0</text>
	</g>
	<g class="ev ev-2">
		<text class="t" x="14" y="218">12:04:41</text>
		<text class="t strong" x="80" y="218">allocation.ready</text>
		<text class="t" x="240" y="218">billing-api/api#1</text>
	</g>
	<g class="ev ev-3">
		<text class="t" x="14" y="236">12:04:44</text>
		<text class="t on-text" x="80" y="236">deployment.succeeded</text>
		<text class="t" x="240" y="236">billing-api gen 7</text>
	</g>

	<!-- footer -->
	<rect class="bar" x="8" y="270" width="424" height="18" />
	<text class="t" x="14" y="283">↑↓ navigate · enter open · ? help</text>
	<text class="t" x="426" y="283" text-anchor="end">updated 1s ago</text>
</svg>

<style>
	.light {
		fill: var(--edge);
	}

	.bar {
		fill: color-mix(in srgb, var(--fg) 7%, var(--bg));
	}

	.t {
		font-size: 11px;
	}

	.on-text {
		fill: var(--accent);
	}

	.b {
		font-weight: 600;
	}

	.ev {
		animation: 8s ease-out infinite both;
	}

	.ev-0 {
		animation-name: ev-0;
	}

	.ev-1 {
		animation-name: ev-1;
	}

	.ev-2 {
		animation-name: ev-2;
	}

	.ev-3 {
		animation-name: ev-3;
	}

	@keyframes ev-0 {
		0%,
		4% {
			opacity: 0;
			transform: translateY(4px);
		}
		8%,
		90% {
			opacity: 1;
			transform: none;
		}
		97%,
		100% {
			opacity: 0;
		}
	}

	@keyframes ev-1 {
		0%,
		20% {
			opacity: 0;
			transform: translateY(4px);
		}
		24%,
		90% {
			opacity: 1;
			transform: none;
		}
		97%,
		100% {
			opacity: 0;
		}
	}

	@keyframes ev-2 {
		0%,
		36% {
			opacity: 0;
			transform: translateY(4px);
		}
		40%,
		90% {
			opacity: 1;
			transform: none;
		}
		97%,
		100% {
			opacity: 0;
		}
	}

	@keyframes ev-3 {
		0%,
		52% {
			opacity: 0;
			transform: translateY(4px);
		}
		56%,
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
