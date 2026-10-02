# Changelog

## [0.3.0] - 2026-10-02

Pre-built Docker images are now available to run and deploy gateway instances immediately.

### What's New

- Adds base Docker image

### Technical Changes

- Improves documentation with a Getting Started guide and updated usage examples

## [0.2.0] - 2026-09-27

In addition to one-off scripts, the gateway now supports streaming CDP commands over WebSockets for live, interactive sessions.

### What's New

- Adds the `GET /ws` endpoint to run interactive Chrome DevTools Protocol (CDP) sessions over WebSockets

### Technical Changes

- Improves documentation with CDP usage instructions

## 0.1.0 - 2026-09-26

`lpgw` is a gateway service for running Lightpanda workloads in private infrastructure with process lifecycle and concurrency control.

[0.2.0]: https://github.com/florianilch/lightpanda-gateway/compare/v0.1.0...v0.2.0
[0.3.0]: https://github.com/florianilch/lightpanda-gateway/compare/v0.2.0...v0.3.0
