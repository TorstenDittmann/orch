import type { Application, Comparison, DatabaseBackups, Deployment, JobRun, LogBatch, LogSource, NodeUsageHistory, OneOffJob, Snapshot, VolumeBackup } from './types';

export class ApiError extends Error {
	constructor(message: string, readonly status: number) {
		super(message);
	}
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
	const response = await fetch(path, { credentials: 'same-origin', ...init });
	const body = await response.json().catch(() => ({ error: response.statusText }));
	if (!response.ok) throw new ApiError(body.error || response.statusText, response.status);
	return body as T;
}

function post<T>(path: string, body: unknown = {}): Promise<T> {
	return request<T>(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}

const segment = encodeURIComponent;

export const api = {
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
	databaseBackups: (app: string, database: string) =>
		request<DatabaseBackups>(`/api/apps/${segment(app)}/databases/${segment(database)}/backups`),
	backupDatabase: (app: string, database: string) => post<VolumeBackup>(`/api/apps/${segment(app)}/databases/${segment(database)}/backup`),
	restoreDatabase: (app: string, database: string, backup: string) =>
		post<{ restored: string; previous: VolumeBackup | null }>(`/api/apps/${segment(app)}/databases/${segment(database)}/restore`, { backup }),
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
