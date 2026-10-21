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
make release VERSION=v0.4.0 packages the Linux binaries with dashboards,
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
The user subsequently installed the edge image and updated the dashboard.

## Dashboard query audit

Both dashboard formats contain the same 64 PromQL expressions. All were accepted
by the deployment Prometheus during the query audit. Over a two-day range, health,
latency and failed-probe ratios retained identical values after removing redundant
vector matching. The simplified quality expression matched the original values
within floating-point tolerance. Removing the selection role filter recovered
older label-less history as separate series. Deviation now shows its raw gauge
rather than an interval average.

A two-day query at a 15-second step reproduced the 11,000-point resolution error;
a 30-second step succeeded. Both formats now use a 5,000-point target while
preserving minimum intervals. This verification covers the checked queries and
deployment, not every Grafana version or arbitrary date range.

## Source contracts and discovery

Tests cover fixed three-source diagnostics, gRPC probing without balancers or
configuration, explicit expvars access despite API/configuration failures, valid
empty observation/traffic groups, and contract failures. Discovery tests cover
IPv4/IPv6 wildcard normalization, concrete addresses, invalid listeners, override
precedence, concurrent endpoints, and removal of configuration without reuse of
old addresses or samples. The edge build was verified alongside v0.3.0 on the live NAS.

## v0.4.0 release preparation

Race-detector tests, go vet, native macOS and static Linux amd64/arm64 builds,
license generation and release archive packaging passed locally. All 64 current
dashboard expressions were accepted by the deployment Prometheus. The two formats
have identical query multisets. Overview, runtime memory and collection screenshots
were refreshed from Grafana with private addresses excluded by cropping.
The edge image for commit 4cabf26 was run in an isolated temporary container
sharing the existing Xray network namespace, with XRAY_METRICS_URL empty and
its test listener bound only to loopback. All three source statuses were 1.
Baseline v0.3.0 and the new build each exposed 123 samples; excluding build-info
and duration series, their series/label sets matched exactly. Repeated scrapes
also succeeded. The temporary container and private environment file were removed;
the production exporter and Xray were not restarted.
