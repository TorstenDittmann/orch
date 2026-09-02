# ADR 0005: begin with one SQLite controller

Status: Accepted by Plan #1.

v0.1 uses one SQLite WAL database and makes no control-plane HA claim. Agents
cache assignments and continue supervising existing workloads while the
controller is unavailable. DNS and ingress also retain their last valid state.

This removes consensus operations from the first product test. Persistence is
kept behind an internal interface, and three-controller HA requires a separate
plan backed by measured demand.
