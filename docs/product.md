# Limitations

Review these limits before choosing orch for a production workload.

## Suitability

orch is a good fit when:

- you operate Ubuntu 24.04 LTS or Fedora 44 hosts;
- one team controls the cluster and its applications;
- workers can reach each other over private IPv4;
- applications run as OCI containers;
- a single durable controller is acceptable;
- local persistent data can remain tied to one worker.

## Product limits

orch does not provide:

- Kubernetes APIs or compatibility;
- a highly available or multi-region control plane;
- automatic fencing during network partitions;
- distributed or replicated storage;
- autoscaling;
- Windows workers;
- arbitrary schedulers, plugins, or runtime drivers.

The controller stores desired state and makes scheduling decisions. If it is
unavailable, agents keep their assigned workloads running, but deployments and
cluster changes wait.

Local volumes stay on the worker where they were created. orch will not start
an empty replacement volume elsewhere after that worker is lost. Back up
stateful applications with tools appropriate to the application.

## Responsibilities

orch installs and manages its own services, but you remain responsible for:

- host and provider firewall policy;
- DNS and public routing;
- operating-system security updates;
- access to the hosts;
- backup retention outside the controller;
- monitoring application behavior beyond declared health checks.

Start with the [installation guide](installation.md), or review
[security and recovery](security.md) before choosing production workloads.
