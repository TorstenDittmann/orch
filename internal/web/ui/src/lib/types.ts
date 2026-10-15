// Shapes of the protojson payloads served by internal/web. 64-bit integers
// arrive as strings and enums as their full proto names.
export type Timestamp = string | null;

export interface Cluster {
	id: string;
	name: string;
	endpoint: string;
	generation: string;
	createdAt: Timestamp;
	workloadCidr: string;
	networkGeneration: string;
	networkFrozen: boolean;
}

export interface ApplicationSummary {
	name: string;
	generation: string;
	revisionId: string;
	executionStatus: string;
	observedGeneration: string;
	activeGeneration: string;
	executionReason: string;
	executionMessage: string;
	createdAt: Timestamp;
	updatedAt: Timestamp;
}

export interface Allocation {
	id: string;
	service: string;
	replica: number;
	phase: string;
	healthStatus: string;
	reason: string;
	message: string;
	restartCount: number;
	address: string;
	nodeName: string;
	placementReason: string;
	readyAt: Timestamp;
	updatedAt: Timestamp;
	hasExitCode: boolean;
	exitCode: number;
}

export interface Route {
	service: string;
	domain: string;
	https: boolean;
	state: string;
	certificateState: string;
	upstreamCount: number;
	reason: string;
	message: string;
	nodeName: string;
}

export interface TCPRoute {
	service: string;
	publicPort: number;
	targetPort: number;
	state: string;
	upstreamCount: number;
	message: string;
	nodeName: string;
}

export interface CronJob {
	job: string;
	nextRunAt: Timestamp;
	activeRunId: string;
	lastRunId: string;
	lastState: string;
	reason: string;
	message: string;
}

export interface Application extends ApplicationSummary {
	digest: string;
	canonicalManifestJson: string;
	allocations: Allocation[];
	deleted: boolean;
	activeRevisionId: string;
	routes: Route[];
	tcpRoutes: TCPRoute[];
	cronJobs: CronJob[];
}

export interface DeploymentImage {
	requestedImage: string;
	kind: string;
	name: string;
	image: string;
}

export interface Deployment {
	application: string;
	revisionId: string;
	generation: string;
	digest: string;
	createdAt: Timestamp;
	rollbackFromRevisionId: string;
	outcome: string;
	executionStatus: string;
	reason: string;
	message: string;
	finishedAt: Timestamp;
	current: boolean;
	active: boolean;
	images: DeploymentImage[];
}

export interface DeploymentChange {
	kind: string;
	path: string;
	before: string;
	after: string;
	redacted: boolean;
}

export interface Comparison {
	from: Deployment;
	to: Deployment;
	changes: DeploymentChange[];
	truncated: boolean;
}

export interface Node {
	id: string;
	name: string;
	connectionStatus: string;
	createdAt: Timestamp;
	lastSeenAt: Timestamp;
	operatingSystem: string;
	architecture: string;
	runtimeVersion: string;
	snapshotter: string;
	cgroupsV2: boolean;
	nodeSubnet: string;
	networkState: string;
	networkReason: string;
	cniVersion: string;
	dnsState: string;
	caddyVersion: string;
	ingressState: string;
	drainingObjects: number;
	lifecycle: string;
	labels: Record<string, string>;
	ingress: boolean;
	totalCpuMillis: string;
	totalMemoryBytes: string;
	allocatableCpuMillis: string;
	allocatableMemoryBytes: string;
	assignedCpuMillis: string;
	assignedMemoryBytes: string;
	advertiseAddress: string;
	wireguardListenPort: number;
	drainBlockers: string[];
	certificateNotAfter: Timestamp;
	certificateState: string;
	certificateReason: string;
}

export interface Runtime {
	id: string;
	kind: string;
	application: string;
	resource: string;
	replica: number;
	runId: string;
	nodeName: string;
	nodeConnection: string;
	allocationPhase: string;
	jobRunState: string;
	healthStatus: string;
	restartCount: number;
	exitCode: number;
	hasExitCode: boolean;
	address: string;
	cpuMillis: string;
	memoryBytes: string;
	reason: string;
	message: string;
	startedAt: Timestamp;
	finishedAt: Timestamp;
	updatedAt: Timestamp;
}

export interface Event {
	sequence: string;
	kind: string;
	payloadJson: string;
	createdAt: Timestamp;
}

export interface JobRun {
	id: string;
	application: string;
	job: string;
	scheduledAt: Timestamp;
	state: string;
	exitCode: number;
	hasExitCode: boolean;
	startedAt: Timestamp;
	finishedAt: Timestamp;
	reason: string;
	message: string;
}

export interface OneOffJob {
	id: string;
	application: string;
	service: string;
	revisionId: string;
	image: string;
	kind: string;
	nodeId: string;
	retryOf: string;
	cancelRequested: boolean;
	status: JobRun;
}

export interface Snapshot {
	target: string;
	transport: string;
	refreshedAt: string;
	cluster?: Cluster;
	applications?: ApplicationSummary[];
	runtimes?: { runtimes: Runtime[]; truncated: boolean };
	nodes?: Node[];
	events?: { events: Event[] };
	errors: string[];
}

export interface LogLine {
	stream: string;
	text: string;
}

export interface LogBatch {
	lines: LogLine[];
	nextCursor: string;
	cursorReset: boolean;
	exited: boolean;
}

export type LogSource =
	| { kind: 'service'; app: string; service: string; replica: number }
	| { kind: 'run'; app: string; job: string; run: string; label?: string };
