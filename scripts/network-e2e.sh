#!/usr/bin/env bash
set -euo pipefail

repository_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repository_dir"

if [[ "$(uname -s)" != "Linux" ]]; then
	echo "network E2E requires a disposable Linux host" >&2
	exit 2
fi
if [[ "$(id -u)" != "0" ]]; then
	echo "network E2E requires root" >&2
	exit 2
fi
for command_name in mise ip; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		echo "required command is unavailable: $command_name" >&2
		exit 2
	fi
done

caddy_binary="${ORCH_CADDY_BINARY:-/usr/bin/caddy}"
caddy_user="${ORCH_CADDY_USER:-orch-ingress}"
pebble_binary="${ORCH_PEBBLE_BINARY:-$(mise which pebble 2>/dev/null || true)}"
if [[ ! -x "$caddy_binary" ]]; then
	echo "Caddy executable is unavailable: $caddy_binary" >&2
	exit 2
fi
if ! id "$caddy_user" >/dev/null 2>&1; then
	echo "dedicated Caddy user is unavailable: $caddy_user" >&2
	exit 2
fi
if [[ -z "$pebble_binary" || ! -x "$pebble_binary" ]]; then
	echo "pinned Pebble executable is unavailable; run mise install" >&2
	exit 2
fi
caddy_version="$($caddy_binary version)"
if [[ ! "$caddy_version" =~ ^v2\.11\.3([[:space:]]|$) ]]; then
	echo "Caddy v2.11.3 is required; found: $caddy_version" >&2
	exit 2
fi

backend_cidr="${ORCH_CADDY_BACKEND_CIDR:-10.254.254.1/32}"
backend_address="${backend_cidr%/*}"
if ip -o -4 address show | awk '{print $4}' | {
	while IFS= read -r address; do
		[[ "$address" == "$backend_cidr" ]] && exit 0
	done
	exit 1
}; then
	echo "refusing to reuse existing private test address: $backend_cidr" >&2
	exit 2
fi

cleanup() {
	set +e
	ip address delete "$backend_cidr" dev lo >/dev/null 2>&1
}
trap cleanup EXIT
ip address add "$backend_cidr" dev lo label lo:orch-e2e

./scripts/containerd-e2e.sh

ORCH_CADDY_E2E=1 \
ORCH_CADDY_BINARY="$caddy_binary" \
ORCH_CADDY_USER="$caddy_user" \
ORCH_CADDY_BACKEND_ADDRESS="$backend_address" \
	ORCH_PEBBLE_BINARY="$pebble_binary" \
	GOTOOLCHAIN=local mise exec -- go test -v -count=1 \
	-run '^TestCaddyReal(HTTPReloadRollbackAndAdoption|HTTPSWithIsolatedPebble|WildcardHTTPSWithOnDemandPebble)$' ./internal/ingress
