# ADR 0004: use a dedicated containerd runtime

Status: Accepted by Plan #1; runtime contract implemented by Plan #4.

Agents will execute OCI images through a pinned containerd/runc installation
and a narrow internal Runtime interface. They will not depend on Docker Engine
or implement the Docker Swarm API.

A dedicated namespace and tested bundle make ownership and upgrades explicit.
The interface also permits deterministic tests without a privileged runtime.

The first executable slice pins the Go client and external daemon contract to
containerd 2.3.4. It requires `containerd-shim-runc-v2`, `runc`, cgroups v2,
Linux amd64 or arm64, and an explicitly configured snapshotter. The repository
does not install or manage those host components yet.

Every allocation uses an opaque deterministic object/snapshot ID, complete
ownership labels, and a persistent per-allocation containerd lease. The agent
refuses label mismatches. OCI specs use isolated namespaces, no capabilities,
no-new-privileges, device denial, CPU/memory limits, and only declared
agent-managed bind mounts. Workload networking is intentionally absent until
the networking Plan.
