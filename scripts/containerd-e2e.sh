#!/usr/bin/env bash
set -euo pipefail

repository_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repository_dir"

if [[ "$(uname -s)" != "Linux" ]]; then
	echo "containerd E2E requires a dedicated Linux host" >&2
	exit 2
fi
if [[ "$(id -u)" != "0" ]]; then
	echo "containerd E2E requires root" >&2
	exit 2
fi
for command_name in mise containerd containerd-shim-runc-v2 runc ctr; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		echo "required command is unavailable: $command_name" >&2
		exit 2
	fi
done

containerd_socket="${ORCH_CONTAINERD_SOCKET:-/run/containerd/containerd.sock}"
snapshotter="${ORCH_CONTAINERD_SNAPSHOTTER:-overlayfs}"
if [[ ! -S "$containerd_socket" ]]; then
	echo "containerd socket is unavailable: $containerd_socket" >&2
	exit 2
fi
containerd_version="$(containerd --version)"
if [[ ! "$containerd_version" =~ (^|[[:space:]])v?2\.3\.4([[:space:]]|$) ]]; then
	echo "containerd 2.3.4 is required; found: $containerd_version" >&2
	exit 2
fi

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/orch-containerd-e2e.XXXXXX")"
namespace_file="$work_dir/namespace"

cleanup_namespace() {
	if [[ ! -s "$namespace_file" ]]; then
		return
	fi
	local namespace
	namespace="$(tr -d '\r\n' < "$namespace_file")"
	case "$namespace" in
		orch-????????-????-4???-[89ab]???-????????????) ;;
		*)
			echo "refusing cleanup of unexpected namespace: $namespace" >&2
			return
			;;
	esac
	while IFS= read -r container_id; do
		[[ -n "$container_id" ]] || continue
		ctr --address "$containerd_socket" --namespace "$namespace" tasks kill --signal SIGKILL "$container_id" >/dev/null 2>&1 || true
		ctr --address "$containerd_socket" --namespace "$namespace" tasks rm --force "$container_id" >/dev/null 2>&1 || true
		ctr --address "$containerd_socket" --namespace "$namespace" containers rm "$container_id" >/dev/null 2>&1 || true
	done < <(ctr --address "$containerd_socket" --namespace "$namespace" containers list --quiet 2>/dev/null || true)
	while IFS= read -r image_name; do
		[[ -n "$image_name" ]] || continue
		ctr --address "$containerd_socket" --namespace "$namespace" images rm "$image_name" >/dev/null 2>&1 || true
	done < <(ctr --address "$containerd_socket" --namespace "$namespace" images list --quiet 2>/dev/null || true)
	ctr --address "$containerd_socket" namespaces rm "$namespace" >/dev/null 2>&1 || true
}

cleanup() {
	set +e
	cleanup_namespace
	case "$work_dir" in
		"${TMPDIR:-/tmp}"/orch-containerd-e2e.*) rm -rf -- "$work_dir" ;;
		*) echo "refusing cleanup of unexpected work directory: $work_dir" >&2 ;;
	esac
}
trap cleanup EXIT

mise install
go_architecture="$(GOTOOLCHAIN=local mise exec -- go env GOARCH)"
case "$go_architecture" in
	amd64|arm64) ;;
	*)
		echo "containerd E2E supports Linux amd64 and arm64; found: $go_architecture" >&2
		exit 2
		;;
esac

GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH="$go_architecture" \
	mise exec -- go build -trimpath -o "$work_dir/orch" ./cmd/orch
GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH="$go_architecture" \
	mise exec -- go build -trimpath -o "$work_dir/orch-fixture" ./test/runtime-fixture
image_reference="$(GOTOOLCHAIN=local mise exec -- go run ./test/oci-fixture \
	--binary "$work_dir/orch-fixture" \
	--output "$work_dir/orch-fixture.oci.tar" \
	--name registry.invalid/orch-e2e \
	--architecture "$go_architecture")"

ORCH_CONTAINERD_E2E=1 \
ORCH_CONTAINERD_SOCKET="$containerd_socket" \
ORCH_CONTAINERD_SNAPSHOTTER="$snapshotter" \
ORCH_E2E_IMAGE_ARCHIVE="$work_dir/orch-fixture.oci.tar" \
ORCH_E2E_IMAGE_REFERENCE="$image_reference" \
ORCH_E2E_ORCH_BINARY="$work_dir/orch" \
ORCH_E2E_NAMESPACE_FILE="$namespace_file" \
	GOTOOLCHAIN=local mise exec -- go test -v -count=1 \
	-run '^TestContainerdSingleNodeLifecycle$' ./internal/e2e
