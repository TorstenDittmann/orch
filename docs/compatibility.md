# Compatibility

From orch 1.0, these promises hold for every 1.x release. Anything not listed
here, such as the agent protocol, the controller's database layout or the
exact wording of command output, may change between releases.

## Manifests

A manifest that amends `https://orch.torsten.work/pkl/v1/Application.pkl` and
passes `orch validate` keeps validating and deploying the same way with every
later 1.x release. New releases may add optional fields with defaults that
keep the old behaviour; they do not remove or rename fields, change a default,
or make a valid manifest invalid.

Manifests written before 1.0 amend `pkl/v1alpha1/Application.pkl`. That file
is v1 under its old name, so the same promise covers them.

## Command line and API

- Commands and flags documented in the [CLI reference](cli.md) keep working.
  `--json` output keeps its fields; new fields may appear.
- A 1.x `orch` CLI works with any 1.x controller for the commands both
  versions have.
- The [MCP server](ai.md) keeps its tool names and arguments. New tools and
  optional arguments may appear.
- Administrator contexts, access tokens, GitHub trust and targets created by
  one 1.x release keep working with the next.

## Clusters and upgrades

- `orch cluster upgrade` upgrades a cluster from any earlier 1.x release, and
  from the last 0.1 release, in one step. Workers follow on their own, and
  workloads keep running throughout.
- Controller backups and off-site backup sets restore with the same or any
  later 1.x release.
- A 1.x controller keeps workers on older 1.x agents running during a rolling
  upgrade. Manifest fields that need a newer agent wait for it, as described
  in [cluster operations](operations.md#upgrades).
- Host systems supported by 1.0 (Ubuntu 24.04 LTS or newer, Fedora 42 or
  newer, amd64 and arm64) stay supported in 1.x while their distribution
  supports them. See [hosts and ports](host.md).

## Deprecations

Something deprecated in 1.x keeps working until 2.0, prints a warning that says
what to use instead, and is listed in the release notes of the release that
deprecates it.
