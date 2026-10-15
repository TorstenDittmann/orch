import { api } from './api';
import { label } from './format';
import { ask } from './state.svelte';
import type { Node } from './types';

export interface NodeAction {
	id: string;
	label: string;
	danger?: boolean;
	run: () => void;
}

export function nodeActions(node: Node): NodeAction[] {
	const lifecycle = label(node.lifecycle);
	const actions: NodeAction[] = [];
	const action = (id: string, text: string, title: string, body: string, success: string, danger = false) =>
		actions.push({
			id,
			label: text,
			danger,
			run: () => ask({ title, body, action: text, danger, success, run: () => api.updateNode(node.name, id) })
		});
	if (lifecycle === 'active') action('cordon', 'Cordon', `Cordon ${node.name}?`, 'The scheduler stops placing new workloads on this node. Running workloads keep running.', `${node.name} cordoned`);
	if (lifecycle === 'cordoned') action('uncordon', 'Uncordon', `Uncordon ${node.name}?`, 'The scheduler can place workloads on this node again.', `${node.name} uncordoned`);
	if (lifecycle === 'active' || lifecycle === 'cordoned')
		action('drain', 'Drain', `Drain ${node.name}?`, 'Workloads are moved off this node. Local-volume and cron workloads follow stricter handoff rules and may block the drain.', `Draining ${node.name}`, true);
	if (node.ingress) action('ingress-off', 'Disable ingress', `Disable ingress on ${node.name}?`, 'This node stops serving HTTP and TCP routes.', `Ingress disabled on ${node.name}`, true);
	else action('ingress-on', 'Enable ingress', `Enable ingress on ${node.name}?`, 'This node starts serving HTTP and TCP routes.', `Ingress enabled on ${node.name}`);
	return actions;
}
