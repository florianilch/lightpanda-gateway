# Landlock Example

This example extends the gateway with a small [`go-landlock`](https://github.com/landlock-lsm/go-landlock) wrapper. Requires Landlock ABI v3 or newer enabled in the host kernel and permitted by the container runtime.

_Landlock does not cover every filesystem operation or revoke already-open file descriptors, so adapt and test for your deployment._
