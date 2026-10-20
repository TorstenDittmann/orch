import type { Application, Comparison, Deployment, JobRun, LogBatch, LogSource, NodeUsageHistory, OneOffJob, Snapshot } from './types';

export class ApiError extends Error {
	constructor(message: string, readonly status: number) {
		super(message);
	}
}

// In orch panel, dashboard requests name the selected cluster.
export const selection = { cluster: '' };

async function request<T>(path: string, init?: RequestInit): Promise<T> {
	const headers = new Headers(init?.headers);
	if (selection.cluster && path.startsWith('/api/')) headers.set('X-Orch-Cluster', selection.cluster);
	const response = await fetch(path, { credentials: 'same-origin', ...init, headers });
	const body = await response.json().catch(() => ({ error: response.statusText }));
	if (!response.ok) throw new ApiError(body.error || response.statusText, response.status);
	return body as T;
}

function post<T>(path: string, body: unknown = {}): Promise<T> {
	return request<T>(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}

const segment = encodeURIComponent;

export interface PanelCluster {
	name: string;
	endpoint: string;
	error?: string;
}

export const api = {
	clusters: () => request<{ clusters: PanelCluster[] }>('/panel/api/clusters'),
	connectCode: () => post<{ code: string; expiresAt: string; command: string }>('/panel/api/clusters/code'),
	disconnect: (name: string) => post<{ name: string; revoked: boolean }>(`/panel/api/clusters/${segment(name)}/disconnect`),
	snapshot: () => request<Snapshot>('/api/snapshot'),
	application: (name: string) =>
		request<{ application: Application; deployments: { deployments: Deployment[] } }>(`/api/apps/${segment(name)}`),
	compare: (name: string, from: string, to: string) =>
		request<Comparison>(`/api/apps/${segment(name)}/compare?${new URLSearchParams({ from, to })}`),
	cronRuns: (app: string, job: string) => request<{ runs: JobRun[] }>(`/api/apps/${segment(app)}/cron/${segment(job)}/runs`),
	jobs: (app = '', before = '') => request<{ jobs: OneOffJob[]; nextBeforeId: string }>(`/api/jobs?${new URLSearchParams({ app, before })}`),
	jobAction: (id: string, action: 'cancel' | 'retry') => post<OneOffJob>(`/api/jobs/${segment(id)}/${action}`),
	nodeUsage: (name: string) => request<NodeUsageHistory>(`/api/nodes/${segment(name)}/usage`),
	updateNode: (name: string, action: string) => post(`/api/nodes/${segment(name)}`, { action }),
	rollback: (app: string, revisionId: string, expectedGeneration: string) =>
		post(`/api/apps/${segment(app)}/rollback`, { revisionId, expectedGeneration }),
	logs(source: LogSource, cursor: string, previous = false) {
		const params = new URLSearchParams({ app: source.app, cursor });
		if (source.kind === 'run') {
			params.set('job', source.job);
			params.set('run', source.run);
		} else {
			params.set('service', source.service);
			params.set('replica', String(source.replica));
			if (previous) params.set('previous', '1');
		}
		return request<LogBatch>(`/api/logs?${params}`);
	}
};
