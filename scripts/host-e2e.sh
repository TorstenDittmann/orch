#!/usr/bin/env bash
# Exercises orch end to end on this host: controller install, a worker on the
# same host, ingress, a deployment served on port 80, a one-off job and, with a
# second binary, a cluster upgrade that must keep the app serving.
#
#   scripts/host-e2e.sh ORCH [UPGRADE_ORCH]
#
# It installs systemd services, containerd and firewall rules and does not
# clean up. Run it only on a disposable machine such as a CI runner.
set -euo pipefail

[[ $# -ge 1 && $# -le 2 ]] || { echo "usage: $0 ORCH [UPGRADE_ORCH]" >&2; exit 2; }
# orch only runs binaries that are root owned and not group or world
# writable, so stage both where an installer would put them.
bin=/usr/local/lib/orch-e2e
sudo install -D -o root -g root -m 0755 "$1" "$bin/orch"
orch="$bin/orch"
upgrade=""
if [[ $# -eq 2 ]]; then
	sudo install -D -o root -g root -m 0755 "$2" "$bin/orch-upgrade"
	upgrade="$bin/orch-upgrade"
fi
pkl="$(command -v pkl)" || { echo "pkl is required on PATH" >&2; exit 2; }
work="$(mktemp -d "${TMPDIR:-/tmp}/orch-host-e2e.XXXXXX")"
chmod 0755 "$work"

as_root() { sudo env PATH="$(dirname "$pkl"):/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" "$@"; }
step() { printf '\n==> %s\n' "$*"; }
json_field() { python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }

serving() {
	local deadline=$((SECONDS + ${1:-120}))
	until [[ "$(curl -fsS --max-time 5 http://127.0.0.1/healthz 2>/dev/null)" == ok ]]; do
		if ((SECONDS > deadline)); then
			echo "the app is not serving on port 80" >&2
			return 1
		fi
		sleep 2
	done
}

diagnose() {
	set +e
	step "Diagnostics"
	as_root "$orch" nodes
	as_root "$orch" app status e2e
	as_root "$orch" events --limit 50
	sudo journalctl -u orch-controller -u orch-agent -u orch-containerd --no-pager -n 200
}
trap 'diagnose' ERR

step "Install the controller with $("$orch" version)"
as_root "$orch" init --name e2e --wait-timeout 5m

step "Join this host as a worker"
token="$(as_root "$orch" join-token e2e-node --json | json_field token)"
as_root "$orch" join --wait-timeout 5m "$token"
as_root "$orch" node ingress e2e-node --enable

step "Deploy an app behind the public listener"
cat >"$work/orch.pkl" <<'PKL'
amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "e2e"

services {
  ["web"] {
    image = "docker.io/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0"
    command { "/bin/sh" }
    args { "-c"; "mkdir -p /www; echo ok >/www/healthz; exec httpd -f -p 8080 -h /www" }
    resources { cpu = "100m"; memory = "64Mi" }
    http = new Http { port = 8080; public = true; healthPath = "/healthz" }
  }
}
PKL
as_root "$orch" deploy --root "$work" --file "$work/orch.pkl" --wait --timeout 10m
serving

step "Run a one-off job"
as_root "$orch" job run e2e web --wait -- /bin/sh -c 'echo e2e-job'

if [[ -n "$upgrade" ]]; then
	want="$("$upgrade" version)"
	step "Upgrade the cluster to $want"
	as_root "$upgrade" cluster upgrade --binary "$upgrade"
	deadline=$((SECONDS + 600))
	until as_root "$upgrade" nodes --json | python3 -c '
import json, sys
nodes = json.load(sys.stdin)
nodes = nodes.get("nodes", nodes) if isinstance(nodes, dict) else nodes
sys.exit(0 if nodes and all((n.get("agent_version") or n.get("agentVersion")) == sys.argv[1] for n in nodes) else 1)
' "$want"; do
		if ((SECONDS > deadline)); then
			echo "workers did not reach $want" >&2
			false
		fi
		sleep 5
	done
	serving
	orch="$upgrade"

	step "Redeploy the unchanged pre-1.0 manifest with the new version"
	as_root "$orch" deploy --root "$work" --file "$work/orch.pkl" --wait --timeout 10m
	serving
fi

step "Passed"
