# Verification

## Automated checks

make test runs the race detector and go vet. Tests cover panel authentication,
settings response decoding, disabled routing rules, subscription outbounds,
selector membership, primary/fallback/override roles for leastLoad and leastPing,
unknown selection for random/roundRobin, overrides for all four strategies,
absent balancers, collection failures, loopback fallback and cycles,
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

## Balancer strategy coverage

leastLoad is the production-verified selection strategy. leastPing selection is
supported by the pinned Xray API and automated tests, but has not been verified
in the production deployment. random and roundRobin provide candidate lists,
not observable per-connection choices: primary and fallback selection metrics
are omitted without an active override. Tests cover both nonempty and empty
candidate lists and explicit override activity for these strategies. Structural
availability remains available and must not be read as actual selection or health.

## Panel status and parallel API collection

Automated tests cover authenticated status GET requests, partial/missing fields,
all three process states, malformed/unknown state, response size limits, independent
settings/status failures, and the N+4 request budget. Deterministic timing tests
verify that status overlaps configuration and runtime requests, API duration spans
both API calls without summing them, and gRPC/expvars do not wait for status.

Read-only verification of the installed panel confirmed the existing Bearer token
can call server/status and receive panelVersion 3.10.2, Xray version 26.10.10,
and state running. No service restart or routing mutation was used for this check.
The new exporter binary has not yet been deployed for an end-to-end live test.
