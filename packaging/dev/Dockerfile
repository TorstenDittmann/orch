# The orch-dev image runs a one-node orch cluster (controller, agent,
# containerd and Caddy) inside a privileged container for `orch dev`.
# `make dev-image` stages the build context in dist/dev and builds orch-dev:local.
ARG BASE=ubuntu:24.04

FROM ${BASE} AS artifacts
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
# The same pinned upstream releases as packaging/node/artifacts.json, for both architectures.
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) containerd=1d0eccd52471ab75e8d4645b538f73dc635f905cd2be776366eb1036a0089595; \
             runc=f6ae8efc0fa40079e1475e97cbe9d1bd3f106a28d6af78a11d9f1bd565515e60; \
             cni=b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303; \
             caddy=3894577b14657feab3624d782f64175050211e52a228a6f57b4f24f4b0d970f3 ;; \
      arm64) containerd=ad21f456a69bca9c183a8bde892a7576d5d09ae88b3e376a47bde4a886037ab3; \
             runc=633301e2e32f8a5ad54031aab4901eb00308bec677dd15faa2751e8f9dab5ca4; \
             cni=56171987d3947707c3563db2f4001bccaf50fd63468611b9f3cbecb1375ee7ec; \
             caddy=866d2b226a4c07adc38aea7df653ee63b98890691d4cd41db1f1c83e18300bbe ;; \
      *) echo "unsupported architecture $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    fetch() { curl -fsSL -o "$1" "$2" && echo "$3  $1" | sha256sum -c -; }; \
    mkdir -p /opt/orch/bin /opt/orch/cni /tmp/a && cd /tmp/a; \
    fetch containerd.tgz "https://github.com/containerd/containerd/releases/download/v2.3.4/containerd-static-2.3.4-linux-$TARGETARCH.tar.gz" "$containerd"; \
    tar -xzf containerd.tgz -C /opt/orch bin/containerd bin/containerd-shim-runc-v2 bin/ctr; \
    fetch runc "https://github.com/opencontainers/runc/releases/download/v1.4.3/runc.$TARGETARCH" "$runc"; \
    install -m 0755 runc /opt/orch/bin/runc; \
    fetch cni.tgz "https://github.com/containernetworking/plugins/releases/download/v1.9.1/cni-plugins-linux-$TARGETARCH-v1.9.1.tgz" "$cni"; \
    tar -xzf cni.tgz -C /opt/orch/cni ./loopback ./bridge ./host-local; \
    fetch caddy.tgz "https://github.com/caddyserver/caddy/releases/download/v2.11.3/caddy_2.11.3_linux_$TARGETARCH.tar.gz" "$caddy"; \
    tar -xzf caddy.tgz -C /opt/orch/bin caddy; \
    rm -rf /tmp/a

FROM ${BASE}
ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates iproute2 nftables libcap2-bin kmod \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --no-create-home --shell /usr/sbin/nologin orch-ingress
COPY --from=artifacts /opt/orch /opt/orch
COPY orch-linux-${TARGETARCH} /opt/orch/bin/orch
COPY containerd.toml /opt/orch/containerd.toml
COPY entrypoint.sh /opt/orch/bin/orch-dev-entrypoint
RUN chown -R root:root /opt/orch && chmod -R go-w /opt/orch && setcap cap_net_bind_service=+ep /opt/orch/bin/caddy && ln -s /opt/orch/bin/orch /usr/local/bin/orch
ENV PATH=/opt/orch/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
VOLUME /var/lib/orch
STOPSIGNAL SIGTERM
ENTRYPOINT ["/opt/orch/bin/orch-dev-entrypoint"]
