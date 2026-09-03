import { job, run } from '@change-codes/ci';

export default job({
	name: 'Go checks',
	steps: [
		run('Verify change.codes checkout', 'test -n "$CHANGE_CODES_SHA"'),
		run('Install pinned toolchain', 'mise install'),
		run('Download dependencies', 'GOTOOLCHAIN=local mise exec -- go mod download'),
		run('Run project checks', 'GOTOOLCHAIN=local mise exec -- make check'),
		run('Build Linux amd64', 'GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 CGO_ENABLED=0 mise exec -- go build -trimpath ./cmd/orch'),
		run('Compile Linux amd64 network tests', 'GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 CGO_ENABLED=0 mise exec -- go test -c -o /tmp/orch-network-amd64.test ./internal/network'),
		run('Build Linux arm64', 'GOTOOLCHAIN=local GOOS=linux GOARCH=arm64 CGO_ENABLED=0 mise exec -- go build -trimpath ./cmd/orch'),
		run('Compile Linux arm64 network tests', 'GOTOOLCHAIN=local GOOS=linux GOARCH=arm64 CGO_ENABLED=0 mise exec -- go test -c -o /tmp/orch-network-arm64.test ./internal/network')
	]
});
