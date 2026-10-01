# syntax=docker/dockerfile:1

ARG BASE_IMAGE=ghcr.io/florianilch/lightpanda-gateway:base

FROM lightpanda/browser:0.4.0@sha256:01eb5d37ba537259d60ebfe32be49d3db4c7b6d1daac23b59b8532126fe30df0 AS lightpanda
ADD --chmod=0644 --checksum=sha256:8486a10c4393cee1c25392769ddd3b2d6c242d6ec7928e1414efff7dfb2f07ef \
    https://raw.githubusercontent.com/lightpanda-io/browser/refs/tags/0.4.0/LICENSE \
    /usr/share/doc/lightpanda/LICENSE

FROM ${BASE_IMAGE}

COPY --from=lightpanda /bin/lightpanda /usr/local/bin/lightpanda
COPY --from=lightpanda /usr/share/doc/lightpanda/LICENSE /usr/share/doc/lightpanda/LICENSE
ENV LIGHTPANDA_DISABLE_CORE_DUMP=1

LABEL org.opencontainers.image.title="Gateway for Lightpanda" \
    org.opencontainers.image.description="A gateway for running Lightpanda workloads with process lifecycle and concurrency control." \
    org.opencontainers.image.licenses="MIT AND AGPL-3.0-only"
