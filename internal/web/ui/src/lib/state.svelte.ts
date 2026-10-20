import { api, ApiError, selection, type PanelCluster } from './api';
import type { LogSource, Snapshot } from './types';

// panel is set when the dashboard is served by orch panel, which manages
// several clusters; orch web serves one and has no panel API.
export const panel = $state({
	enabled: false,
	clusters: [] as PanelCluster[],
	current: ''
});

function storedCluster(): string {
	try {
		return localStorage.getItem('orch-cluster') ?? '';
	} catch {
		return '';
	}
}

export async function loadClusters() {
	try {
		const { clusters } = await api.clusters();
		// orch web answers unknown paths with the app shell, not a list.
		if (!Array.isArray(clusters)) return;
		panel.enabled = true;
		panel.clusters = clusters;
		const names = clusters.map((cluster) => cluster.name);
		if (!names.includes(panel.current)) {
			const stored = storedCluster();
			selectCluster(names.includes(stored) ? stored : (names[0] ?? ''), false);
		}
	} catch {
		panel.enabled = false;
	}
}

export function selectCluster(name: string, reload = true) {
	panel.current = name;
	selection.cluster = name;
	try {
		localStorage.setItem('orch-cluster', name);
	} catch {
		// Storage can be unavailable; the selection still applies for this session.
	}
	if (reload) {
		live.snapshot = null;
		live.loading = true;
		refresh();
	}
}

export const live = $state({
	snapshot: null as Snapshot | null,
	error: '',
	unauthorized: false,
	loading: true,
	now: Date.now(),
	paused: false
});

let timer: ReturnType<typeof setTimeout> | undefined;

export async function refresh() {
	clearTimeout(timer);
	try {
		live.snapshot = await api.snapshot();
		live.error = '';
		live.unauthorized = false;
	} catch (error) {
		live.error = (error as Error).message;
		live.unauthorized = error instanceof ApiError && error.status === 401;
	} finally {
		live.loading = false;
		live.now = Date.now();
		if (!live.unauthorized) timer = setTimeout(refresh, document.hidden || live.paused ? 10000 : 2000);
	}
}

document.addEventListener('visibilitychange', () => {
	if (!document.hidden) refresh();
});

export interface Toast {
	id: number;
	tone: 'ok' | 'bad' | 'info';
	text: string;
}

export const toasts = $state<Toast[]>([]);
let toastId = 0;

export function toast(text: string, kind: Toast['tone'] = 'info') {
	const id = ++toastId;
	toasts.push({ id, tone: kind, text });
	setTimeout(() => dismiss(id), kind === 'bad' ? 8000 : 4000);
}

export function dismiss(id: number) {
	const index = toasts.findIndex((item) => item.id === id);
	if (index >= 0) toasts.splice(index, 1);
}

export interface Confirmation {
	title: string;
	body: string;
	action: string;
	danger?: boolean;
	run: () => Promise<unknown>;
	success: string;
}

export const ui = $state({
	confirm: null as Confirmation | null,
	busy: false,
	palette: false,
	log: null as LogSource | null,
	sidebar: false
});

export function ask(confirmation: Confirmation) {
	ui.confirm = confirmation;
}

export async function confirmPending() {
	const pending = ui.confirm;
	if (!pending || ui.busy) return;
	ui.busy = true;
	try {
		await pending.run();
		toast(pending.success, 'ok');
		ui.confirm = null;
		refresh();
	} catch (error) {
		toast((error as Error).message, 'bad');
	} finally {
		ui.busy = false;
	}
}

export function openLogs(source: LogSource) {
	ui.log = source;
}

type Theme = 'system' | 'light' | 'dark';

function storedTheme(): Theme {
	try {
		const value = localStorage.getItem('orch-theme');
		return value === 'light' || value === 'dark' ? value : 'system';
	} catch {
		return 'system';
	}
}

export const theme = $state({ value: storedTheme() });

export function applyTheme(value: Theme) {
	theme.value = value;
	if (value === 'system') delete document.documentElement.dataset.theme;
	else document.documentElement.dataset.theme = value;
	try {
		localStorage.setItem('orch-theme', value);
	} catch {
		// Storage can be unavailable; the theme still applies for this session.
	}
}
