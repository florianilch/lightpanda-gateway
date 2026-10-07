# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27.1 AS wrapper-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /lightpanda-landlock .
RUN mkdir -m 0700 /startup-out

FROM ghcr.io/florianilch/lightpanda-gateway:standalone-distroless

COPY --from=wrapper-build /lightpanda-landlock /usr/local/bin/lightpanda-landlock
COPY --from=wrapper-build --chown=nonroot:nonroot /startup-out/ /app/out/

CMD [ "--addr=:8080", "--browser-binary=/usr/local/bin/lightpanda-landlock" ]
