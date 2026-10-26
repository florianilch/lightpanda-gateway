# syntax=docker/dockerfile:1

ARG BASE_IMAGE=ghcr.io/florianilch/lightpanda-gateway:base

FROM lightpanda/browser:1.0.2@sha256:5d1d724c476957d8c2f8393a51fd089477523b0e5aa9c783c48688785d934fb9 AS lightpanda
ADD --chmod=0644 --checksum=sha256:8486a10c4393cee1c25392769ddd3b2d6c242d6ec7928e1414efff7dfb2f07ef \
    https://raw.githubusercontent.com/lightpanda-io/browser/refs/tags/1.0.2/LICENSE \
    /usr/share/doc/lightpanda/LICENSE

FROM ${BASE_IMAGE} AS lpgw

FROM gcr.io/distroless/base-nossl-debian13:nonroot@sha256:6a2a3346335b94d3be0732472245f42ca3ce3cc1135a9521a2bda4b677f2ef0d

WORKDIR /app

COPY --from=lightpanda /bin/lightpanda /usr/local/bin/lightpanda
COPY --from=lightpanda /usr/share/doc/lightpanda/LICENSE /usr/share/doc/lightpanda/LICENSE
ENV LIGHTPANDA_DISABLE_CORE_DUMP=1

COPY --from=lpgw /usr/local/bin/lpgw /usr/local/bin/lpgw

USER nonroot:nonroot

EXPOSE 8080/tcp

ENTRYPOINT [ "/usr/local/bin/lpgw" ]
CMD [ "--addr=:8080" ]

LABEL org.opencontainers.image.title="Gateway for Lightpanda" \
    org.opencontainers.image.description="A gateway for running Lightpanda workloads with process lifecycle and concurrency control." \
    org.opencontainers.image.licenses="MIT AND AGPL-3.0-only"
