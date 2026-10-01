# syntax=docker/dockerfile:1

FROM scratch AS bin
COPY --from=lpgw-amd64 /lpgw /linux/amd64/lpgw
COPY --from=lpgw-arm64 /lpgw /linux/arm64/lpgw

FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a

RUN groupadd --system lpgw \
    && useradd --system --gid lpgw --no-create-home --shell /usr/sbin/nologin lpgw

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
RUN chown lpgw:lpgw /app

ARG TARGETPLATFORM
COPY --from=bin /${TARGETPLATFORM}/lpgw /usr/local/bin/lpgw

USER lpgw

EXPOSE 8080/tcp

ENTRYPOINT [ "/usr/local/bin/lpgw" ]
CMD [ "--addr=:8080" ]

LABEL org.opencontainers.image.title="Gateway for Lightpanda" \
    org.opencontainers.image.description="A gateway for running Lightpanda workloads with process lifecycle and concurrency control." \
    org.opencontainers.image.licenses="MIT"
