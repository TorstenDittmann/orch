import { job, run } from '@change-codes/ci';

export default job({
	name: 'Go checks',
	steps: [
		run('Verify change.codes checkout', 'test -n "$CHANGE_CODES_SHA"'),
		run('Install pinned toolchain', 'mise install'),
		run('Download dependencies', 'GOTOOLCHAIN=local mise exec -- go mod download'),
		run('Run project checks', 'GOTOOLCHAIN=local mise exec -- make check'),
		run('Build Linux amd64', 'GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 CGO_ENABLED=0 mise exec -- go build -trimpath ./cmd/orch'),
		run('Build Linux arm64', 'GOTOOLCHAIN=local GOOS=linux GOARCH=arm64 CGO_ENABLED=0 mise exec -- go build -trimpath ./cmd/orch')
	]
});
