# Comparison rubric

This document defines the benchmark before results exist. It must not be
mistaken for measured evidence.

Use one reference application with two long-running services, one public HTTP
route, one secret, a health check, and two web replicas. Run each platform on
the same three clean VMs.

Measure:

- Minutes and operator actions from clean hosts to valid HTTPS.
- Number of configuration files, non-comment lines, and platform concepts.
- Extra mandatory services and long-running system components.
- Behavior during a bad rollout, task crash, worker loss, and control-plane
  outage.
- Whether local state behavior is explicit before deployment.
- Steps needed to inspect cause, roll back, back up control state, and recover.

Baselines:

- Docker Compose represents single-host authoring.
- Docker Swarm already offers desired state, discovery, mTLS, load balancing,
  and rolling updates; matching those features is not differentiation.
- k3s packages Kubernetes more compactly but retains Kubernetes APIs and object
  concepts.
- Nomad provides an established client/server scheduler and is the strongest
  lightweight-orchestrator comparison.

Record actual commands, timings, versions, failures, and host sizes when the
benchmark environment is available. Do not fill the result table from memory
or marketing pages.

| Platform | Time to HTTPS | Operator actions | Config lines | Mandatory extras | Failure notes |
| --- | ---: | ---: | ---: | --- | --- |
| Compose | pending | pending | pending | pending | pending |
| Swarm | pending | pending | pending | pending | pending |
| k3s | pending | pending | pending | pending | pending |
| Nomad | pending | pending | pending | pending | pending |
| orch | pending | pending | pending | pending | pending |

Primary references:

- https://docs.docker.com/engine/swarm/
- https://docs.k3s.io/architecture
- https://developer.hashicorp.com/nomad/docs/architecture
