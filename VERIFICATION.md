# Local verification

Completed on macOS ARM64, 2026-10-14:

* Go 1.27.2, Xray API packages pinned to v26.9.30 commit b26a91de4f32.
* `go test -race ./...` passed, including in-memory real gRPC bindings and
  local HTTP API responses, failures, timeouts, concurrent scrapes, selector
  membership, overrides, fallback cycles and optional observation fields.
* `go vet ./...` passed.
* macOS ARM64 binary compiled and `--version` executed.
* Linux amd64 and arm64 binaries compiled, both statically linked ELF.
* OrbStack built `3x-ui-exporter:dev` and ran it with read-only filesystem,
  all capabilities dropped, no-new-privileges, unprivileged image user.
* Native and container HTTP /metrics smoke checks returned 200 and both source
  status gauges 0 for deliberately unreachable test sources. No application
  measurements were synthesized.
* Container output passed `promtool check metrics`, using official Prometheus
  image digest sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e.
* Temporary native server and test exporter container were stopped.

Live read-only checks through a user-approved temporary gRPC relay confirmed:

* HandlerService.ListOutbounds: 12 runtime outbounds.
* StatsService.QueryStats(reset=false): 26 inbound/outbound traffic samples.
* StatsService.GetSysStats: successful response.
* RoutingService.GetBalancerInfo: two current targets for ishosting-ch.
* ObservatoryService.GetOutboundStatus: UNIMPLEMENTED, service not registered.
* 3x-ui API request from existing exporter namespace using its environment token:
  success=true; discovered awg-premium and ishosting-ch, burstObservatory present.
  API services are HandlerService, LoggerService, StatsService, RoutingService.
  Token value was neither printed nor stored locally.

After the user enabled ObservatoryService and restarted the core, live checks
against Xray v26.10.10 all passed. Observatory returned 64 converted samples,
traffic returned 26 samples, HandlerService returned 12 runtime outbounds,
GetSysStats succeeded, and the user-created someTest balancer returned one target.
The opt-in live test now takes its balancer tag from XUI_TEST_BALANCER (someTest
by default) and fails if the runtime balancer is absent. The temporary proxy was
removed again after testing; port 16279 is closed.

Pending: combined end-to-end live HTTP exporter verification, dashboard UI
import/rendering, GitHub/GHCR publication, license choice.
Existing Python exporter and Xray were not modified or stopped. Remote 11111 is
HTTP expvars now supplies observatory data (see update below).

## HTTP observatory migration

Observatory collection now uses `/debug/vars`, with a separate `expvars` source
status and duration. gRPC no longer invokes or requires ObservatoryService.

* Race tests and vet passed, including HTTP ordinary/burst data, omitted protobuf
  defaults, empty/missing observations, malformed responses, source isolation,
  disabled metrics, duplicate tags and deadlines.
* Read-only live expvars test against 192.168.10.16:11111 passed: 8 outbounds and
  59 metric samples at the moment checked (missing RTT measurements stay absent).
* Mac ARM64 and static Linux amd64/arm64 binaries rebuilt.
* Docker image 3x-ui-exporter:dev rebuilt, ID 547711b91506.
* No running TrueNAS services or Xray configuration were changed.

The earlier container/promtool smoke checks predate this change; combined live
API + gRPC + expvars /metrics verification remains pending.

## Duration spike investigation (2026-10-15)

Analyzed 6723 points per source from Prometheus (approximately 9h20m).
No up/source-success failures. API median 13.53ms, p95 15.51ms, p99 18.12ms,
max 235.60ms; four points over 100ms. Expvars median 1.19ms, max 71.55ms.
Old grpc timing included API/expvars waiting: median 15.19ms, max 238.13ms.
Four additional grpc peaks exceeded 100ms with fast API/expvars; exact RPC
responsible remains unproven. Largest peaks had no Xray GC pause increment.

Corrected grpc duration to count active phases only. Added slow-operation
warnings at 100ms. Deterministic fake-clock regression tests cover slow API,
slow expvars, simultaneous slow dependencies and concurrent grpc phases.
This correction is local; the mounted TrueNAS binary needs manual replacement
and restarting only the exporter container. Historical measurements retain
the previous timing semantics.

## Single expvars snapshot (2026-10-15)

Removed GetSysStats and uptime/goroutine metrics. Traffic, eight memory/GC
metrics and observations share one HTTP snapshot. QueryStats remains a
fallback for unavailable traffic. Tests count requests and cover disabled
HTTP, HTTP errors, malformed individual groups, missing versus zero fields,
original tags, ignored user traffic and preserved independent groups.
The v2 dashboard copy removes uptime/goroutine panels and keeps memory/GC.
The existing TrueNAS deployment is unchanged until manual binary replacement.

Latest refactor validation passed: race tests and vet; native Mac and static
Linux amd64/arm64 builds; live expvars returned 26 traffic samples, 8 memory/GC
samples and 64 observation samples. Native /metrics smoke used real expvars
and deliberately unreachable API/gRPC: traffic/memory/observations were
published, no unnecessary grpc call was attempted, and removed metrics were
absent. Output passed promtool check metrics. Docker image rebuilt as
3x-ui-exporter:dev (4e57f0aa2a95) and unprivileged read-only startup passed.
All temporary native smoke processes were stopped.

## Explicit source ownership (2026-10-15)

Removed fallback QueryStats, StatsService client and gRPC traffic collection.
Traffic is decoded only from expvars; missing, malformed, disabled or unreachable
HTTP traffic never triggers another source. Tests cover absent traffic while
balancer data remains available, plus valid groups surviving malformed siblings.
Historical fallback behavior above describes older revisions only.

## Lightweight settings API (2026-10-15)

Replaced GET getConfigJson with POST /panel/api/xray/. Regression tests cover
Bearer authentication, HTTP method/path, encoded and object envelopes, invalid
templates, subscription outbounds, disabled routing rules and loopback fallback.
make test build linux passed (race detector, vet, macOS and Linux amd64/arm64).
The new endpoint has not yet been measured or verified against the deployed panel.
