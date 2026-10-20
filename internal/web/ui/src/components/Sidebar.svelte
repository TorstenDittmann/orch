<script lang="ts">
	import { Activity, AppWindow, Boxes, Briefcase, Earth, LayoutDashboard, Monitor, Moon, Search, Server, Sun } from '@lucide/svelte';
	import { tone } from '../lib/format';
	import { router } from '../lib/router.svelte';
	import { applyTheme, live, panel, selectCluster, theme, ui } from '../lib/state.svelte';

	const snapshot = $derived(live.snapshot);
	const apps = $derived(snapshot?.applications ?? []);
	const nodes = $derived(snapshot?.nodes ?? []);
	const runtimes = $derived(snapshot?.runtimes?.runtimes ?? []);
	const badApps = $derived(apps.filter((app) => tone(app.executionStatus) === 'bad').length);
	const badNodes = $derived(nodes.filter((node) => tone(node.connectionStatus) === 'bad').length);

	const items = $derived([
		{ path: '', label: 'Overview', icon: LayoutDashboard, count: undefined, alert: 0 },
		{ path: 'apps', label: 'Applications', icon: AppWindow, count: apps.length, alert: badApps },
		{ path: 'runtimes', label: 'Runtimes', icon: Boxes, count: runtimes.length, alert: 0 },
		{ path: 'nodes', label: 'Nodes', icon: Server, count: nodes.length, alert: badNodes },
		{ path: 'jobs', label: 'Jobs', icon: Briefcase, count: undefined, alert: 0 },
		{ path: 'events', label: 'Events', icon: Activity, count: undefined, alert: 0 },
		...(panel.enabled ? [{ path: 'clusters', label: 'Clusters', icon: Earth, count: panel.clusters.length, alert: panel.clusters.filter((cluster) => cluster.error).length }] : [])
	]);

	const current = $derived(router.route.parts[0] ?? '');
	const themes = [
		{ value: 'system', icon: Monitor, label: 'System theme' },
		{ value: 'light', icon: Sun, label: 'Light theme' },
		{ value: 'dark', icon: Moon, label: 'Dark theme' }
	] as const;
</script>

<aside class:open={ui.sidebar}>
	<div class="brand">
		<div class="logo" aria-hidden="true">
			<svg viewBox="0 0 24 24" width="18" height="18"><circle cx="12" cy="12" r="4" fill="currentColor" /><circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="2" stroke-dasharray="4 3" /></svg>
		</div>
		{#if panel.enabled}
			<label class="cluster switcher">
				<span class="target faint">Cluster</span>
				<select
					aria-label="Cluster"
					value={panel.current}
					onchange={(event) => {
						const value = (event.currentTarget as HTMLSelectElement).value;
						if (value === '') location.hash = '#/clusters';
						else selectCluster(value);
					}}
				>
					{#each panel.clusters as cluster (cluster.name)}
						<option value={cluster.name}>{cluster.name}</option>
					{/each}
					<option value="">Manage clusters…</option>
				</select>
			</label>
		{:else}
			<div class="cluster">
				<div class="name">{snapshot?.cluster?.name || 'orch'}</div>
				<div class="target faint">{snapshot ? `${snapshot.target || 'context'} · ${snapshot.transport}` : 'connecting…'}</div>
			</div>
		{/if}
	</div>

	<button class="search" onclick={() => (ui.palette = true)}>
		<Search size={14} />
		<span>Search</span>
		<kbd>⌘K</kbd>
	</button>

	<nav>
		{#each items as item (item.path)}
			<a href="#/{item.path}" class:active={current === item.path} onclick={() => (ui.sidebar = false)}>
				<item.icon size={16} />
				<span class="text">{item.label}</span>
				{#if item.alert}
					<span class="alert" title="{item.alert} need attention">{item.alert}</span>
				{:else if item.count !== undefined}
					<span class="count">{item.count}</span>
				{/if}
			</a>
		{/each}
	</nav>

	<div class="foot">
		<div class="live" class:down={!!live.error}>
			<span class="beacon"></span>
			{live.error ? 'Disconnected' : 'Live'}
		</div>
		<div class="themes" role="radiogroup" aria-label="Theme">
			{#each themes as option (option.value)}
				<button
					role="radio"
					aria-checked={theme.value === option.value}
					aria-label={option.label}
					title={option.label}
					class:on={theme.value === option.value}
					onclick={() => applyTheme(option.value)}
				>
					<option.icon size={14} />
				</button>
			{/each}
		</div>
	</div>
</aside>
{#if ui.sidebar}
	<button class="scrim" aria-label="Close menu" onclick={() => (ui.sidebar = false)}></button>
{/if}

<style>
	aside {
		position: fixed;
		inset: 0 auto 0 0;
		width: var(--sidebar);
		background: var(--surface);
		border-right: 1px solid var(--border);
		display: flex;
		flex-direction: column;
		padding: 14px 10px;
		z-index: 30;
	}
	.brand {
		display: flex;
		align-items: center;
		gap: 10px;
		padding: 4px 8px 14px;
	}
	.logo {
		width: 32px;
		height: 32px;
		border-radius: 9px;
		display: grid;
		place-items: center;
		color: #fff;
		background: linear-gradient(135deg, var(--accent), color-mix(in srgb, var(--accent) 60%, #22d3ee));
		box-shadow: 0 4px 12px -4px var(--accent);
		flex: none;
	}
	.cluster {
		min-width: 0;
	}
	.switcher {
		flex: 1;
		display: flex;
		flex-direction: column;
	}
	.switcher select {
		width: 100%;
		font: inherit;
		font-weight: 650;
		color: var(--text);
		background: transparent;
		border: 0;
		padding: 0;
		cursor: pointer;
		text-overflow: ellipsis;
	}
	.switcher select:focus-visible {
		outline: 2px solid var(--accent);
		border-radius: 4px;
	}
	.name {
		font-weight: 650;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.target {
		font-size: 12px;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.search {
		display: flex;
		align-items: center;
		gap: 8px;
		margin: 0 4px 12px;
		height: 32px;
		padding: 0 8px 0 10px;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		background: var(--surface-2);
		color: var(--text-3);
		cursor: pointer;
		font-size: 13px;
	}
	.search span {
		flex: 1;
		text-align: left;
	}
	.search:hover {
		border-color: var(--border-strong);
	}
	nav {
		display: flex;
		flex-direction: column;
		gap: 1px;
		flex: 1;
	}
	nav a {
		display: flex;
		align-items: center;
		gap: 10px;
		height: 34px;
		padding: 0 10px;
		border-radius: var(--radius-sm);
		color: var(--text-2);
		font-weight: 500;
	}
	nav a:hover {
		background: var(--surface-2);
		color: var(--text);
	}
	nav a.active {
		background: var(--accent-soft);
		color: var(--accent-text);
	}
	.text {
		flex: 1;
	}
	.count {
		font-size: 12px;
		color: var(--text-3);
		font-variant-numeric: tabular-nums;
	}
	.alert {
		font-size: 11px;
		font-weight: 600;
		min-width: 18px;
		height: 18px;
		border-radius: 999px;
		display: grid;
		place-items: center;
		padding: 0 5px;
		background: var(--bad);
		color: #fff;
	}
	.foot {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: 10px 6px 0;
		border-top: 1px solid var(--border);
	}
	.live {
		display: flex;
		align-items: center;
		gap: 7px;
		font-size: 12.5px;
		color: var(--text-2);
	}
	.beacon {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: var(--ok);
		box-shadow: 0 0 0 3px var(--ok-soft);
	}
	.down .beacon {
		background: var(--bad);
		box-shadow: 0 0 0 3px var(--bad-soft);
	}
	.themes {
		display: flex;
		background: var(--surface-2);
		border-radius: var(--radius-sm);
		padding: 2px;
		gap: 2px;
	}
	.themes button {
		width: 26px;
		height: 24px;
		display: grid;
		place-items: center;
		border: 0;
		border-radius: 4px;
		background: none;
		color: var(--text-3);
		cursor: pointer;
	}
	.themes button.on {
		background: var(--surface);
		color: var(--text);
		box-shadow: var(--shadow-sm);
	}
	.scrim {
		display: none;
	}
	@media (max-width: 900px) {
		aside {
			transform: translateX(-100%);
			transition: transform 0.2s ease;
			box-shadow: var(--shadow-lg);
		}
		aside.open {
			transform: none;
		}
		.scrim {
			display: block;
			position: fixed;
			inset: 0;
			z-index: 25;
			border: 0;
			background: rgb(0 0 0 / 0.35);
		}
	}
</style>
