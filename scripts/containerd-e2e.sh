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
for command_name in mise containerd containerd-shim-runc-v2 runc ctr nft ip; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		echo "required command is unavailable: $command_name" >&2
		exit 2
	fi
done

if [[ "$(tr -d '[:space:]' </proc/sys/net/ipv4/ip_forward)" != "1" ]]; then
	echo "containerd E2E requires net.ipv4.ip_forward=1" >&2
	exit 2
fi
cni_bin_dir="${ORCH_CNI_BIN_DIR:-/opt/cni/bin}"
for plugin in loopback bridge host-local; do
	if [[ ! -x "$cni_bin_dir/$plugin" ]]; then
		echo "required CNI plugin is unavailable: $cni_bin_dir/$plugin" >&2
		exit 2
	fi
done

caddy_binary="${ORCH_CADDY_BINARY:-/usr/bin/caddy}"
caddy_user="${ORCH_CADDY_USER:-orch-ingress}"
if [[ ! -x "$caddy_binary" ]]; then
	echo "Caddy executable is unavailable: $caddy_binary" >&2
	exit 2
fi
if ! id "$caddy_user" >/dev/null 2>&1; then
	echo "dedicated Caddy user is unavailable: $caddy_user" >&2
	exit 2
fi
caddy_version="$($caddy_binary version)"
if [[ ! "$caddy_version" =~ ^v2\.11\.3([[:space:]]|$) ]]; then
	echo "Caddy v2.11.3 is required; found: $caddy_version" >&2
	exit 2
fi
caddy_uid="$(id -u "$caddy_user")"
caddy_gid="$(id -g "$caddy_user")"

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
chmod 0711 "$work_dir"
ingress_data_dir="$work_dir/ingress"
install -d -m 0700 -o "$caddy_uid" -g "$caddy_gid" "$ingress_data_dir"
namespace_file="$work_dir/namespace"
network_file="$work_dir/network.json"
external_namespace="orch-e2e-ext-$$"
external_host_if="oeh$$"
external_peer_if="oep$$"
outbound_pid=""
outbound_ready=0
foreign_nft_table="orch_e2e_foreign_$$"
caddy_cleanup_safe=1

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

cleanup_network() {
	if [[ ! -s "$network_file" ]]; then
		return
	fi
	local table bridge
	table="$(sed -n 's/.*"table":"\([^"]*\)".*/\1/p' "$network_file")"
	bridge="$(sed -n 's/.*"bridge":"\([^"]*\)".*/\1/p' "$network_file")"
	if [[ ! "$table" =~ ^orch_[0-9a-f]{12}$ ]] || [[ ! "$bridge" =~ ^orch[0-9a-f]{11}$ ]]; then
		echo "refusing cleanup of unexpected network resources: table=$table bridge=$bridge" >&2
		return
	fi
	nft delete table ip "$table" >/dev/null 2>&1 || true
	ip link delete "$bridge" >/dev/null 2>&1 || true
}

cleanup_external_network() {
	if [[ "$outbound_pid" =~ ^[1-9][0-9]*$ ]]; then
		kill "$outbound_pid" >/dev/null 2>&1 || true
		wait "$outbound_pid" >/dev/null 2>&1 || true
	fi
	ip netns delete "$external_namespace" >/dev/null 2>&1 || true
	ip link delete "$external_host_if" >/dev/null 2>&1 || true
	nft delete table ip "$foreign_nft_table" >/dev/null 2>&1 || true
}

cleanup_caddy() {
	local pid_file="$ingress_data_dir/caddy.pid"
	if [[ ! -f "$pid_file" ]]; then
		return
	fi
	local pid actual_binary expected_binary effective_uid command_line
	pid="$(tr -d '[:space:]' < "$pid_file")"
	if [[ ! "$pid" =~ ^[1-9][0-9]*$ ]] || ! kill -0 "$pid" >/dev/null 2>&1; then
		return
	fi
	actual_binary="$(readlink -f "/proc/$pid/exe" 2>/dev/null || true)"
	expected_binary="$(readlink -f "$caddy_binary" 2>/dev/null || true)"
	effective_uid="$(awk '$1 == "Uid:" { print $3 }' "/proc/$pid/status" 2>/dev/null || true)"
	command_line="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
	if [[ -z "$expected_binary" || "$actual_binary" != "$expected_binary" || "$effective_uid" != "$caddy_uid" ||
		"$command_line" != *"$ingress_data_dir/config.json"* || "$command_line" != *"$ingress_data_dir/caddy.pid"* ]]; then
		echo "refusing to stop a process that does not match the E2E Caddy identity: pid=$pid" >&2
		caddy_cleanup_safe=0
		return
	fi
	kill -TERM "$pid" >/dev/null 2>&1 || true
	for _ in {1..50}; do
		if ! kill -0 "$pid" >/dev/null 2>&1; then
			return
		fi
		sleep 0.1
	done
	kill -KILL "$pid" >/dev/null 2>&1 || true
}

cleanup() {
	set +e
	cleanup_namespace
	cleanup_network
	cleanup_external_network
	cleanup_caddy
	case "$work_dir:$caddy_cleanup_safe" in
		"${TMPDIR:-/tmp}"/orch-containerd-e2e.*:1) rm -rf -- "$work_dir" ;;
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

if nft list table ip "$foreign_nft_table" >/dev/null 2>&1; then
	echo "refusing to reuse foreign nftables sentinel: $foreign_nft_table" >&2
	exit 2
fi
nft add table ip "$foreign_nft_table"

if ip netns list | awk '{print $1}' | grep -Fx "$external_namespace" >/dev/null 2>&1 ||
	ip link show "$external_host_if" >/dev/null 2>&1; then
	echo "refusing to reuse external E2E network resources" >&2
	exit 2
fi
ip netns add "$external_namespace"
ip link add "$external_host_if" type veth peer name "$external_peer_if"
ip link set "$external_peer_if" netns "$external_namespace"
ip address add 198.18.0.1/30 dev "$external_host_if"
ip link set "$external_host_if" up
ip -n "$external_namespace" link set lo up
ip -n "$external_namespace" address add 198.18.0.2/30 dev "$external_peer_if"
ip -n "$external_namespace" link set "$external_peer_if" up
ip netns exec "$external_namespace" env ORCH_E2E_HTTP_PORT=18080 ORCH_E2E_TOKEN=outbound \
	"$work_dir/orch-fixture" serve outbound >"$work_dir/outbound.log" 2>&1 &
outbound_pid="$!"
for _ in {1..50}; do
	if ip netns exec "$external_namespace" "$work_dir/orch-fixture" fetch http://198.18.0.2:18080/ >/dev/null 2>&1; then
		outbound_ready=1
		break
	fi
	sleep 0.1
done
if [[ "$outbound_ready" != "1" ]] || ! kill -0 "$outbound_pid" >/dev/null 2>&1; then
	echo "external E2E fixture failed to start" >&2
	cat "$work_dir/outbound.log" >&2
	exit 2
fi

ORCH_CONTAINERD_E2E=1 \
ORCH_CONTAINERD_SOCKET="$containerd_socket" \
ORCH_CONTAINERD_SNAPSHOTTER="$snapshotter" \
ORCH_E2E_IMAGE_ARCHIVE="$work_dir/orch-fixture.oci.tar" \
ORCH_E2E_IMAGE_REFERENCE="$image_reference" \
ORCH_E2E_ORCH_BINARY="$work_dir/orch" \
ORCH_E2E_CNI_BIN_DIR="$cni_bin_dir" \
ORCH_CADDY_BINARY="$caddy_binary" \
ORCH_CADDY_USER="$caddy_user" \
ORCH_E2E_INGRESS_DATA_DIR="$ingress_data_dir" \
ORCH_E2E_WORKLOAD_CIDR="${ORCH_E2E_WORKLOAD_CIDR:-10.245.0.0/16}" \
ORCH_E2E_OUTBOUND_URL="http://198.18.0.2:18080/" \
ORCH_E2E_EXTERNAL_NAMESPACE="$external_namespace" \
ORCH_E2E_EXTERNAL_HOST_ADDRESS="198.18.0.1" \
ORCH_E2E_EXTERNAL_CLIENT_ADDRESS="198.18.0.2" \
ORCH_E2E_FIXTURE_BINARY="$work_dir/orch-fixture" \
ORCH_E2E_NAMESPACE_FILE="$namespace_file" \
ORCH_E2E_NETWORK_FILE="$network_file" \
	GOTOOLCHAIN=local mise exec -- go test -v -count=1 \
	-run '^TestContainerdSingleNodeLifecycle$' ./internal/e2e

if ! nft list table ip "$foreign_nft_table" >/dev/null 2>&1; then
	echo "orch removed the foreign nftables sentinel" >&2
	exit 1
fi
