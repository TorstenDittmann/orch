# Networking

Services can communicate inside the cluster or be exposed through HTTP, HTTPS,
or TCP ingress.

## Requirements

Workers must reach each other over their advertised private IPv4 addresses.
Allow UDP 51820 between workers for the WireGuard mesh.

The default workload network is `10.88.0.0/16`. If it overlaps a host, VPN,
or office network, choose another CIDR before deploying applications:

```console
orch network configure --cidr 10.99.0.0/16 --target production
```

The host and provider firewall must allow forwarding between the private
interface, WireGuard, and workload network. On Fedora, configure appropriate
firewalld zones and policies while keeping SELinux enforcing.

## HTTPS

Enable ingress on at least one worker:

```console
orch node ingress worker-1 --enable
```

Point the domain's A record at every ingress node that should receive traffic,
then allow TCP 80 and 443 to those nodes.

Configure the service:

```pkl
http = new Http {
  port = 8080
  domains {
    "api.example.com"
  }
  healthPath = "/healthz"
  https = true
}
```

orch publishes the route after the service becomes healthy. HTTPS certificates
are requested automatically. Confirm that public DNS resolves to an ingress
node and that ports 80 and 443 are reachable so certificate issuance can
succeed.

## HTTP

Set `public = true` to create a catch-all listener on port 80:

```pkl
http = new Http {
  port = 8080
  public = true
  healthPath = "/healthz"
}
```

Only one service in the cluster can own this catch-all route. Domain routes
take precedence when both are configured.

## TCP

```pkl
tcp = new Tcp {
  port = 5432
  publicPort = 15432
}
```

`port` is the container's service port. `publicPort` opens the listed port
on ingress nodes and forwards bytes without HTTP or TLS handling. Allow that
port in host and provider firewalls.

## Service discovery

Every service endpoint receives a private address and a cluster DNS name:

```text
SERVICE.APPLICATION.svc.internal
```

Applications can connect to another service through its cluster name. For
example, a service called `database` in application `shop` is available at:

```text
database.shop.svc.internal
```

Only ready endpoints are returned. During a rolling update, old endpoints are
removed after replacements become ready.

## Troubleshooting

Start with:

```console
orch nodes --target production
orch app status hello --target production
orch events --limit 50 --target production
```

Check that:

- all workers advertise reachable, unique private addresses;
- UDP 51820 is allowed between workers;
- the workload CIDR does not overlap existing routes;
- ingress is enabled on the expected nodes;
- DNS points to those ingress nodes;
- public and host firewalls allow the requested listener ports;
- the application's health path returns success.

Application traffic travels directly through workers and ingress nodes. It does
not pass through the controller.
