const prefixes = /^(EXECUTION_STATUS|NODE_CONNECTION_STATUS|NODE_LIFECYCLE|ALLOCATION_PHASE|HEALTH_STATUS|JOB_RUN_STATE|RUNTIME_KIND|ROUTE_STATE|LOG_STREAM)_/;

export function label(value: string | undefined | null): string {
	if (!value) return '';
	return value.replace(prefixes, '').toLowerCase().replace(/_/g, ' ');
}

export type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'neutral';

export function tone(value: string | undefined | null): Tone {
	const text = label(value);
	if (!text || text === 'unspecified') return 'neutral';
	if (/^(running|connected|active|healthy|ready|succeeded|serving|valid|applied|completed)$/.test(text)) return 'ok';
	if (/(fail|unreachable|unhealthy|timed out|error|blocked|expired|rejected)/.test(text)) return 'bad';
	if (/(pending|starting|degraded|draining|cordoned|stopping|waiting|checking|renew|enrolled|pulling|backoff|disconnected)/.test(text)) return 'warn';
	if (/(skipped|stopped|removed|exited|not configured)/.test(text)) return 'neutral';
	return 'info';
}

export function ago(value: string | null | undefined, now = Date.now()): string {
	if (!value) return '—';
	const time = new Date(value).getTime();
	if (!Number.isFinite(time) || time <= 0) return '—';
	const seconds = Math.round((now - time) / 1000);
	const s = Math.abs(seconds);
	const text = s < 5 ? 'now' : s < 60 ? `${s}s` : s < 3600 ? `${Math.round(s / 60)}m` : s < 86400 ? `${Math.round(s / 3600)}h` : `${Math.round(s / 86400)}d`;
	if (text === 'now') return 'just now';
	return seconds < 0 ? `in ${text}` : `${text} ago`;
}

export function datetime(value: string | null | undefined): string {
	if (!value) return '';
	const date = new Date(value);
	return Number.isFinite(date.getTime()) ? date.toLocaleString() : '';
}

export function bytes(value: string | number | undefined): string {
	let n = Number(value || 0);
	const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
	let i = 0;
	while (n >= 1024 && i < units.length - 1) {
		n /= 1024;
		i++;
	}
	return `${n.toFixed(i && n < 10 ? 1 : 0)} ${units[i]}`;
}

export function cores(millis: string | number | undefined): string {
	const n = Number(millis || 0) / 1000;
	return `${n.toFixed(n < 10 ? 2 : 1)}`;
}

export function ratio(used: string | number | undefined, total: string | number | undefined): number {
	const t = Number(total || 0);
	return t > 0 ? Math.min(1, Number(used || 0) / t) : 0;
}

export function short(value: string | undefined, length = 12): string {
	return (value || '').slice(0, length);
}

export function pretty(json: string): string {
	try {
		return JSON.stringify(JSON.parse(json), null, 2);
	} catch {
		return json;
	}
}
