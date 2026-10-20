# The orch-panel image runs `orch panel serve`: a self-hosted dashboard that
# manages any number of orch clusters. Run it on any Docker host with a
# volume at /data, or deploy it on a cluster with `orch panel install`.
# `make panel-context` stages the build context in dist/panel.
FROM gcr.io/distroless/static-debian12
ARG TARGETARCH
COPY orch-linux-${TARGETARCH} /usr/local/bin/orch
ENV ORCH_PANEL_DATA=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/orch"]
CMD ["panel", "serve"]
