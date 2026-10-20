# The orch-panel image runs `orch panel serve`: the web dashboard as an
# application on the cluster it manages. `orch panel install` deploys it.
# `make panel-context` stages the build context in dist/panel.
FROM gcr.io/distroless/static-debian12
ARG TARGETARCH
COPY orch-linux-${TARGETARCH} /usr/local/bin/orch
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/orch"]
CMD ["panel", "serve"]
