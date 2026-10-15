import { api, ApiError } from './api';
import type { LogSource, Snapshot } from './types';

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
