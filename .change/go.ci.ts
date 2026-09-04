import { job, parallel, run } from '@change.codes/ci';

export default job({
	name: 'Go checks',
	steps: [
		run('Verify change.codes checkout', 'test -n "$CHANGE_CODES_SHA"'),
		run('Install pinned toolchain', 'mise install'),
		run('Download dependencies', 'GOTOOLCHAIN=local mise exec -- go mod download'),
		parallel(
			run('Validate generated sources and formatting', 'GOTOOLCHAIN=local mise exec -- make generate-check fmt-check pkl-test'),
			run('Run Go vet', 'GOTOOLCHAIN=local mise exec -- make vet'),
			run('Run Go tests', 'GOTOOLCHAIN=local mise exec -- make test'),
			run('Run Go race tests', 'GOTOOLCHAIN=local mise exec -- make test-race'),
			run('Build Linux amd64', 'GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 CGO_ENABLED=0 mise exec -- go build -trimpath -o /tmp/orch-linux-amd64 ./cmd/orch'),
			run('Compile Linux amd64 network tests', 'GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 CGO_ENABLED=0 mise exec -- go test -c -o /tmp/orch-network-amd64.test ./internal/network'),
			run('Compile Linux amd64 containerd tests', 'GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 CGO_ENABLED=0 mise exec -- go test -c -o /tmp/orch-containerd-amd64.test ./internal/e2e')
		)
	]
});
