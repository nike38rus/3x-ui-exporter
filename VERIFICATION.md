# Verification

## Automated checks

make test runs the race detector and go vet. Tests cover panel authentication,
settings response decoding, disabled routing rules, subscription outbounds,
selector membership, overrides, absent balancers, loopback fallback and cycles,
ordinary and burst observation, partial/missing data, counter validation,
timeouts, concurrent scrapes, request budgets and independent source timing.
The gRPC binding test uses an in-memory server; no live services are required.

make build linux builds the native binary and static Linux amd64/arm64 binaries.
make release VERSION=v0.1.0 packages the Linux binaries with dashboards,
documentation and license notices, and produces SHA-256 checksums.

## Deployment verification

The exporter runs alongside 3x-ui in Docker on TrueNAS, sharing the Xray network
namespace. Prometheus collected all three sources every five seconds. The
lightweight settings API migration preserved all 115 series and labels in that
installation. Source failures and missing runtime data are covered by automated
tests; stopping production Xray is not required for routine verification.

Observed with Xray 26.10.10. The panel settings API implementation was inspected
in 3x-ui 3.10.0 and 3.10.2. This is a tested scope, not a guarantee for all releases.
See README.md for optional read-only live tests. Live tests never reset counters
or modify routing; do not expose the control API publicly to run them.

Source duration is wall-clock time, not CPU usage. Host power management,
scheduling and networking can affect it.
