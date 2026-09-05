# 021: Guided local node installation

Status: accepted

## Context

Manual agent enrollment leaves runtime dependencies, systemd lifecycle and host
network prerequisites to the operator. A service can be active before its agent
has authenticated or converged network state. Repeated installation must not
create another node identity or take over unrelated host services.

## Decision

Provide local `node doctor` and `node install` commands for Ubuntu 24.04 LTS amd64
and Fedora Server 44 amd64. They do not resolve selected targets. Doctor and
install dry-run inspect the host without consuming join credentials or mutating
it. Reports distinguish failures, warnings and unknown checks; installable
failures identify missing prerequisites that the installer supplies.

Install uses a fixed, checksummed upstream artifact bundle and signed distro
packages, dedicated `/opt/orch` binaries, `orch-containerd.service` and
`orch-agent.service`. Caddy remains an agent-supervised non-root process.
Fedora retains SELinux enforcing and firewalld; owned runtime paths receive
labels from the distribution-maintained container policy. Firewall changes are
explicit operator actions, outside installation.

An exclusive host lock and private, atomic installation journal record intended
content before publication and completed phases after successful execution.
A retry must match the original binary, bundle, OS, address and join artifact.
Enrollment retains the existing durable CSR/idempotency-key retry mechanism.
Identical completed invocations start stopped owned services but never restart
healthy ones. Foreign files/services and changed installed content are refused.
There is no implicit upgrade, manual-state adoption, reset or uninstall.

The agent writes a private, ephemeral local receipt after sending its report for
an authenticated controller assignment. Readiness requires the current assignment
and network generations, ready mesh/DNS, and ingress readiness when assigned.
The installer checks freshness, kernel boot ID, enrolled node ID, and systemd
MainPID. A timeout preserves identity, state and running services for diagnosis
and retry; an active systemd unit alone is insufficient.

Fedora also receives an owned veth `.link` rule preserving CNI-assigned MAC
addresses. Its udev persistent-MAC default otherwise races CNI ADD/CHECK. The
rule matches virtual `veth*` devices, leaves physical interfaces alone and
requires only a configuration reload for future links.

## Consequences

Installation requires root, internet access to signed package repositories and
upstream release assets, and a reachable controller. Peer/provider firewall
reachability cannot be established by local inspection. Real VM installation,
workload, reboot and retry drills are required for each supported distribution;
unit tests do not establish SELinux compatibility.

The support matrix is intentionally explicit. Additional distributions,
architectures, upgrades and removal need separate compatibility validation.
