# ADR 0015: Local targets and built-in operator TUI

## Status

Accepted.

## Context

Passing an administrator context path to every command makes a small-cluster
tool feel like a collection of low-level API utilities. Operators also need to
reach private controllers from a workstation through routes they already own,
including WireGuard and OpenSSH. A browser dashboard would add a second server,
session authentication, deployment surface, and CSRF boundary.

## Decision

The CLI stores versioned, named local targets in the user configuration
directory. Import validates an existing context and copies its CA, certificate,
and key atomically into a private target directory. New cluster initialization
creates and selects its target automatically. The current target is the default
for every remote CLI command; `--target`, `ORCH_TARGET`, and the legacy
`--context` escape hatch provide explicit overrides.

Target transport is one of:

- direct, using the canonical controller URL;
- WireGuard, substituting a dial address routed by an OS-managed interface;
- SSH, using the system OpenSSH client and a loopback-only TCP forward.

The canonical HTTPS URL always supplies HTTP authority and the TLS server name.
Every route therefore retains the existing CA, controller-role, cluster-ID, and
administrator mTLS checks. Orch does not manage workstation WireGuard keys or
interfaces, embed an SSH implementation, change host-key policy, or retrieve
credentials from a remote host.

`orch tui` is part of the existing binary and uses the same resolved client. It
uses a responsive master/detail information architecture, command palette,
fuzzy resource switching, contextual actions, bounded asynchronous snapshots,
safe log rendering, and guarded node mutations. Framework-specific types stay
inside `internal/tui`; policy remains in the controller.

## Consequences

Routine local operation no longer exposes credential paths in shell history or
requires users to remember them. Direct, WireGuard, and SSH access behave as
routes to one authenticated controller identity rather than distinct trust
modes. The local target directory is sensitive administrator material and must
be backed up, copied, and revoked accordingly.

The TUI adds terminal framework dependencies and a UI surface that requires
render, accessibility, performance, and terminal-lifecycle tests. A future web
dashboard remains a separate decision because it requires an explicit browser
security and deployment model.
