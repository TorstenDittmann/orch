# Node bundle

`orch node install` embeds these service/configuration templates and the immutable
artifact manifest. It installs Ubuntu 24.04 LTS amd64 and Fedora 44 amd64.
The existing manual runtime path remains available for other supported hosts.

Artifacts are downloaded directly from the upstream GitHub releases in
`artifacts.json`. Each SHA-256 was checked against the release asset digest and
the downloaded bytes. The containerd **static** build avoids depending on a newer
host glibc. Every selected executable must also be ELF64 x86-64. Downloads are
bounded to 256 MiB, extracted content to 512 MiB, and archives to 1,024 entries.
Links, special files, path traversal, missing executables, duplicate selected
entries, and unexpected files are rejected. No upstream installer script runs.

All four projects use Apache-2.0 licensing. Source release links are included in
the manifest; their source trees contain license and third-party notices. This
repository embeds the manifest, not the downloaded upstream executables.

To verify locally downloaded upstream artifacts without installing anything:

```sh
ORCH_ARTIFACT_DIR=/path/to/downloads go test -v ./internal/host -run TestOfficialArtifacts
```

The service names, runtime socket and state paths are deliberately dedicated to
orch. Caddy is supervised by the agent as `orch-ingress`; there is no separate
Caddy systemd service. Fedora uses the distribution's `container-selinux` policy
and persistent `semanage fcontext` mappings for the owned runtime paths. It does
not disable enforcing mode, alter SELinux booleans, or replace firewalld rules.

A new bundle version or executable is not an implicit upgrade: the installation
journal rejects changed binary or artifact hashes. Upgrade and uninstall flows
require a separate design covering live workloads and persistent state.

On Ubuntu and Fedora, the owned `/etc/systemd/network/10-orch-veth.link` preserves the MAC
addresses CNI records for kernel-created `veth*` devices (driver `veth`). Without
it, udev's persistent-MAC policy can change an address after CNI ADD and make
CNI CHECK reject the attachment. The rule applies to future matching veth pairs,
including those created by other software; it does not match physical NICs or
trigger changes to existing interfaces. Installation reloads udev configuration,
not NetworkManager or firewall rules.
