# ADR 0002: implement the product in Go behind one command

Status: Accepted by Plan #1.

The repository uses Go and builds one orch command with future controller,
agent, and operator subcommands. Go aligns with containerd and CNI libraries,
supports static cross-platform CLI builds, and has mature Linux systems APIs.

Bundled runtime and ingress components may remain separate supervised
processes. "One command" is a user-interface constraint, not a requirement to
rewrite or link every dependency into one executable.
