# 022: Guided controller installation and recovery handoff

Status: accepted

## Context

Workers have a tested local installer, while controller bootstrap still leaves
systemd lifecycle, file ownership, administrator handoff, and recovery material
to the operator. Running installation through sudo must not accidentally place
the administrator target in root's personal configuration.

## Decision

Provide local `cluster doctor` and `cluster install` commands for Ubuntu 24.04
LTS amd64 and Fedora Server 44 amd64. Doctor and dry-run are non-mutating.
Installation copies the invoked executable into a controller-specific prefix,
runs a hardened systemd service as a dedicated account, and retains a private
phase journal. Its paths do not overlap the guided worker bundle.

Bootstrap writes a portable administrator directory rather than modifying a
user target store. Readiness requires the exact installed process to own the
listener and an authenticated request to return the journaled cluster identity.
After readiness, installation creates and validates an immutable day-zero
recovery set containing the standalone database, controller PKI, secret master
key, and hashes. Both outputs require secure off-host transfer.

Matching reruns preserve identity and healthy service PIDs. Changed intent,
manual state, foreign paths, and output replacement are refused. Firewall, DNS,
routing, SELinux mode, and unrelated services remain operator-managed.

## Consequences

The first controller uses an unprivileged port and remains a single control
plane. Controller outages pause mutations while agents preserve cached desired
workloads. The initial recovery set does not provide ongoing backup automation.
Upgrades, reconfiguration, uninstall, HA, and automatic credential transfer are
separate features. Real reboot, recovery, co-location, Ubuntu, and enforcing
Fedora acceptance tests are required before support is claimed.
