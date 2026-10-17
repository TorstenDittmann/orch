#!/bin/bash
# Runs a one-node orch cluster inside the orch-dev container: containerd, the
# controller and an ingress-enabled agent. State lives in /var/lib/orch so it
# survives container restarts and recreation.
set -euo pipefail

state=/var/lib/orch
log() { echo "orch-dev: $*" >&2; }

# cgroup v2 forbids processes in a cgroup that delegates controllers, so move
# this container's processes into a leaf first (the same step docker-in-docker takes).
if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
	mkdir -p /sys/fs/cgroup/init
	xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
	sed -e 's/ / +/g' -e 's/^/+/' </sys/fs/cgroup/cgroup.controllers >/sys/fs/cgroup/cgroup.subtree_control
fi
sysctl -qw net.ipv4.ip_forward=1
modprobe -q overlay 2>/dev/null || true
modprobe -q wireguard 2>/dev/null || true

mkdir -p "$state" /run/orch-containerd /var/lib/orch-ingress
chmod 0700 "$state" /run/orch-containerd
chown orch-ingress:orch-ingress /var/lib/orch-ingress
chmod 0700 /var/lib/orch-ingress

pids=()
stop() {
	kill -TERM "${pids[@]}" 2>/dev/null || true
	wait || true
	exit "${1:-0}"
}
trap stop TERM INT

containerd --config /opt/orch/containerd.toml &
pids+=($!)

if [[ ! -f "$state/controller/.initialized" ]]; then
	rm -rf "$state/controller" "$state/admin" "$state/agent"
	mkdir -p "$state/admin" && chmod 0700 "$state/admin"
	orch cluster init --name dev --server https://127.0.0.1:9443 \
		--data-dir "$state/controller" --context "$state/admin/context.json" >/dev/null
	touch "$state/controller/.initialized"
fi

orch controller serve --data-dir "$state/controller" --listen 0.0.0.0:9443 &
pids+=($!)

for _ in $(seq 1 100); do
	if orch status --context "$state/admin/context.json" >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done

if [[ ! -f "$state/agent/.initialized" ]]; then
	rm -rf "$state/agent"
	orch node join create dev --ttl 5m --context "$state/admin/context.json" --output /run/dev.join >/dev/null
	orch agent init --join-file /run/dev.join --data-dir "$state/agent" >/dev/null
	rm -f /run/dev.join
	touch "$state/agent/.initialized"
fi

for _ in $(seq 1 100); do
	[[ -S /run/orch-containerd/containerd.sock ]] && break
	sleep 0.1
done

address="$(ip -4 -o addr show scope global | awk '{ split($4, a, "/"); print a[1]; exit }')"
orch agent serve \
	--data-dir "$state/agent" \
	--advertise-address "$address" \
	--system-reserved-cpu 1m \
	--system-reserved-memory 1Mi \
	--containerd-socket /run/orch-containerd/containerd.sock \
	--snapshotter overlayfs \
	--cni-bin-dir /opt/orch/cni \
	--caddy-binary /opt/orch/bin/caddy \
	--ingress-user orch-ingress \
	--ingress-data-dir /var/lib/orch-ingress &
pids+=($!)

touch /run/orch-dev.ready
log "ready"
# Any component exiting stops the container so the engine's restart policy can bring it back.
wait -n
log "a component exited; stopping"
stop 1
