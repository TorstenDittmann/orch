<script lang="ts">
	import Seo from "../lib/components/Seo.svelte";
	import Agent from "../lib/components/illustrations/Agent.svelte";
	import Database from "../lib/components/illustrations/Database.svelte";
	import Deploy from "../lib/components/illustrations/Deploy.svelte";
	import Network from "../lib/components/illustrations/Network.svelte";
	import Notify from "../lib/components/illustrations/Notify.svelte";
	import Rollout from "../lib/components/illustrations/Rollout.svelte";
	import Setup from "../lib/components/illustrations/Setup.svelte";

	let { data } = $props();
</script>

<Seo title="orch: orchestration for the agentic era" description={data.site.description} />

<section class="container hero">
	<div class="intro">
		<p class="kicker">Orchestration for the agentic era</p>
		<h1>Deploy to your own servers.</h1>
		<p>The simplicity of Docker Swarm with the modern features of Kubernetes. Three commands to a cluster, one file per app, and an API your AI agents can drive safely.</p>
		<div class="actions">
			<a class="button" href="/quick-start/">Get started</a>
			<a class="quiet" href="/docs/">Read the docs</a>
		</div>
	</div>
	<div class="code">{@html data.manifest}</div>
</section>

<section class="container step">
	<div class="intro">
		<h2>Built for agents</h2>
		<p>orch ships an MCP server. Claude Code, Cursor or Codex can read status and logs, deploy and roll back, with a token you can limit to a single app.</p>
		<a class="quiet" href="/ai/">AI assistants →</a>
	</div>
	<Agent />
</section>

<section class="container step">
	<div class="intro">
		<h2>One file, every server</h2>
		<p>Describe the app once and run <code>orch deploy</code>. Replicas are spread across your nodes and turn healthy on their own.</p>
		<a class="quiet" href="/configuration/">Applications →</a>
	</div>
	<Deploy />
</section>

<section class="container step">
	<div class="intro">
		<h2>A cluster in three commands</h2>
		<p>Run <code>orch init</code> on one server and <code>orch join</code> on the rest. Your SSH key is the only credential.</p>
		<a class="quiet" href="/installation/">Installation →</a>
	</div>
	<Setup />
</section>

<section class="container step">
	<div class="intro">
		<h2>Deploys that check themselves</h2>
		<p>Replicas are replaced one at a time and only take traffic once healthy. A failed update rolls back on its own.</p>
		<a class="quiet" href="/runtime/">Deployments →</a>
	</div>
	<Rollout />
</section>

<section class="container step">
	<div class="intro">
		<h2>HTTPS and a private network</h2>
		<p>Ingress nodes get certificates automatically. Every node talks over an encrypted WireGuard mesh.</p>
		<a class="quiet" href="/networking/">Networking →</a>
	</div>
	<Network />
</section>

<section class="container extras">
	<div>
		<Database />
		<h2>Databases</h2>
		<p>Declare Postgres, MySQL, Redis and more next to your services. orch generates the credentials and runs scheduled backups.</p>
		<a class="quiet" href="/databases/">Databases →</a>
	</div>
	<div>
		<Notify />
		<h2>Notifications</h2>
		<p>Failed deploys, backups and cron runs are posted to Slack, Discord or any webhook.</p>
		<a class="quiet" href="/notifications/">Notifications →</a>
	</div>
</section>

<section class="container closing">
	<h2>Between Swarm and Kubernetes</h2>
	<p>Swarm is easy but stopped growing. Kubernetes can do everything but needs a team to run it. orch keeps the easy part and adds what modern apps expect.</p>
	<div class="actions">
		<a class="button" href="/quick-start/">Get started</a>
		<a class="quiet" href="/comparison/">How orch compares →</a>
	</div>
</section>

<style>
	.hero,
	.step {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 64px;
		align-items: center;
	}

	.hero {
		padding-block: 80px 96px;
	}

	.step {
		padding-block: 72px;
		border-top: 1px solid var(--line);
	}

	/* alternate the illustration side down the page */
	.step:nth-of-type(odd) .intro {
		order: 1;
	}

	.hero > *,
	.step > * {
		min-width: 0;
	}

	.hero .intro .kicker {
		margin: 0 0 14px;
		font-family: var(--mono);
		font-size: 13px;
		color: var(--accent);
	}

	h1 {
		margin: 0 0 16px;
		font-size: clamp(34px, 5vw, 44px);
		font-weight: 500;
		line-height: 1.08;
		letter-spacing: -0.04em;
		text-wrap: balance;
	}

	h2 {
		margin: 0 0 12px;
		font-size: 24px;
		font-weight: 500;
		letter-spacing: -0.03em;
		text-wrap: balance;
	}

	.intro p,
	.closing p {
		margin: 0 0 24px;
		max-width: 42ch;
		color: var(--dim);
	}

	.hero .intro p {
		margin-bottom: 28px;
	}

	code {
		font-size: 0.9em;
		color: var(--fg);
	}

	.actions {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 12px 20px;
	}

	.button {
		background: var(--fg);
		color: var(--bg);
		padding: 9px 16px;
		border-radius: 6px;
		font-size: 14px;
		font-weight: 500;
	}

	.button:hover {
		opacity: 0.86;
	}

	.quiet {
		font-size: 14px;
		color: var(--dim);
	}

	.quiet:hover {
		color: var(--fg);
	}

	.extras {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 64px;
		padding-block: 72px;
		border-top: 1px solid var(--line);
	}

	.extras > div {
		min-width: 0;
	}

	.extras h2 {
		margin-top: 40px;
	}

	.extras p {
		margin: 0 0 24px;
		color: var(--dim);
	}

	.closing {
		display: flex;
		flex-direction: column;
		align-items: center;
		text-align: center;
		padding-block: 88px 104px;
		border-top: 1px solid var(--line);
	}

	.closing p {
		max-width: 52ch;
		margin-bottom: 28px;
	}

	@media (max-width: 760px) {
		.hero,
		.step {
			grid-template-columns: 1fr;
			gap: 36px;
		}

		.hero {
			padding-block: 48px 64px;
		}

		.step {
			padding-block: 56px;
		}

		.step:nth-of-type(odd) .intro {
			order: 0;
		}

		.extras {
			grid-template-columns: 1fr;
			gap: 56px;
			padding-block: 56px;
		}

		.extras h2 {
			margin-top: 28px;
		}

		.closing {
			padding-block: 64px 80px;
		}
	}
</style>
