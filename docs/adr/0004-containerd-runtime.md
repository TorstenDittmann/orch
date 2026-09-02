# ADR 0004: use a dedicated containerd runtime

Status: Accepted by Plan #1.

Agents will execute OCI images through a pinned containerd/runc installation
and a narrow internal Runtime interface. They will not depend on Docker Engine
or implement the Docker Swarm API.

A dedicated namespace and tested bundle make ownership and upgrades explicit.
The interface also permits deterministic tests without a privileged runtime.
