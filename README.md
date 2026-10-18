# 3x-ui exporter

## Overview

Synchronous Prometheus exporter for 3x-ui and its running Xray core. One HTTP
API source discovers configuration; gRPC reads runtime outbounds and balancers.
One HTTP expvars response supplies traffic, memory/GC and observatory results. No polling loop, exporter configuration file, or stale cache.

## Disclaimer and maintenance policy

This project is **100% vibe-coded with AI**. It was built for my own setup and
is shared as-is, with no promise of support, ongoing maintenance or future releases.
I may make changes if and when I need them myself.

**Issues will not be read, and pull requests are unlikely to be reviewed or merged.**
Do not rely on this repository for fixes or feature requests. If the project does
not meet your needs, fork it and adapt it for yourself.

## Run

```sh
export XUI_URL=http://panel.example:2053
export XUI_API_TOKEN='<token>'
export XRAY_API=127.0.0.1:62789
export XRAY_METRICS_URL=http://127.0.0.1:11111
export LISTEN_ADDRESS=127.0.0.1:9108
export SCRAPE_TIMEOUT=4s
./bin/3x-ui-exporter
```

All settings have matching CLI flags (`--xui-url`, `--xui-api-token`,
`--xray-api`, `--xray-metrics-url`, `--listen-address`, `--scrape-timeout`). CLI overrides environment.
Prefer an environment variable for the token, to keep it out of process arguments.
The base URL can include a panel base path. HTTP redirects are rejected; secrets
and configuration contents are never logged.

Endpoints: `/metrics`, `/healthz` (exporter liveness only), `/` (landing page).
The collector shares a total deadline per HTTP request. Failed source collection
still returns HTTP 200 with source status and independent successful metrics.
Source startup failures do not terminate the exporter.

Xray gRPC must provide HandlerService and RoutingService. StatsService is not used.
ObservatoryService and GetSysStats are not used. Traffic, memory/GC and observatory results come from
`XRAY_METRICS_URL/debug/vars` (default base URL `http://127.0.0.1:11111`).
Enable Xray metrics in the panel and use a reachable base URL when the exporter
does not share Xray's network namespace. The panel token is never sent to expvars.
If API discovery shows an empty/missing metrics.listen, expvars
collection is disabled: its source gauges and observations are absent.
A successful empty or missing observatory object produces no observations;
HTTP/JSON errors set only expvars source success to zero. Xray traffic policies determine
which counters exist; missing counters are never synthesized. API configuration
is discovery data, not proof that runtime has the same configuration.

## Docker Compose

The example shares the network namespace of an existing Xray/3x-ui container
on the same Docker host. Set XRAY_CONTAINER to its actual container name.
The exporter listens on port 9108 in that namespace; no separate port mapping
is needed. In a bridge deployment, publish 9108 from the Xray container;
with macvlan, use that container IP. After recreating the Xray container, recreate this exporter too so it joins
the current network namespace.

```sh
cp .env.example .env
# Edit .env: set the token, container name and addresses.
docker compose up -d
```

The default image tag is 0.2.0. To use the latest successful main build, set
`EXPORTER_VERSION=edge` in `.env`. The `edge` tag is updated after each
successful main pipeline.
To build your checkout locally:

```sh
docker compose -f compose.yaml -f compose.build.yaml up -d --build
```

When both services belong to the same Compose project, use the **service key**
from `services:` instead of the container name. Add this exporter service to your
existing Compose file (here the panel service key is `x-ui`):

```yaml
services:
  # Your existing x-ui service remains here.
  exporter:
    image: ghcr.io/nike38rus/3x-ui-exporter:0.2.0
    network_mode: service:x-ui
    depends_on:
      x-ui:
        condition: service_started
        restart: true
    environment:
      XUI_URL: http://127.0.0.1:2053
      XUI_API_TOKEN: ${XUI_API_TOKEN:?Set XUI_API_TOKEN}
      XRAY_API: 127.0.0.1:62789
      XRAY_METRICS_URL: http://127.0.0.1:11111
      LISTEN_ADDRESS: :9108
      SCRAPE_TIMEOUT: 4s
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    restart: unless-stopped
```

`network_mode: service:x-ui` establishes startup ordering. The explicit dependency
also requests an exporter restart after Compose-controlled updates/restarts of
the panel; it does not supervise automatic Docker Engine restarts. API readiness
is not required at exporter startup: unavailable sources are retried on each
scrape. Separate Compose projects must use the `container:` example instead.

The example uses port 9108; override LISTEN_ADDRESS in Compose if it conflicts
with another exporter. SCRAPE_TIMEOUT must be shorter than the Prometheus timeout.

```yaml
scrape_configs:
  - job_name: xui-exporter
    scrape_interval: 5s
    scrape_timeout: 5s
    static_configs:
      - targets: ["xray.example:9108"]
```

The metrics endpoint has no authentication. Keep it on a trusted monitoring
network. The panel token needs permission to POST /panel/api/xray/; an admin
API token is supported. Do not expose the Xray gRPC control port to the internet.

## Grafana dashboards

Import grafana-3x-ui.classic.json for the classic dashboard JSON format, or
use grafana-3x-ui.v2.json with Grafana resource schema v2 support. They are two
exports of the same dashboard, not two dashboards to install simultaneously.
Choose your Prometheus datasource on import; the supplied queries expect
job="xui-exporter". Select an instance, then outbound/balancer filters.
Dashboards were exported from Grafana 13.2; older versions are not guaranteed
compatible. The classic format is the more widely supported import option.

Quality scores in Grafana are dashboard heuristics, not the Xray balancer score.
Collection duration stacks API + max(gRPC, expvars); Total is the separate
scrape_duration_seconds measurement made by Prometheus.

## Metrics

Metrics are grouped by the entity they describe. The tables list labels emitted
by the exporter; Prometheus adds scrape labels such as `job` and `instance`.
All durations are in seconds and traffic/memory sizes are in bytes.

Every scrape collects fresh data. Missing or unknown measurements remain absent;
confirmed zeros and false states are published. Counters are cumulative within
an Xray process lifetime and may reset when Xray restarts; use `rate()` or
`increase()` for traffic and allocation rates. Gauges describe current state or
the current observation window.

The screenshots below are actual Grafana captures with anonymized entity names.
Private addresses and browser controls are excluded; measurements are unchanged.
The Balancers screenshots show the v0.2.0 role-aware panels. Quality scores and
other derived values are calculated in Grafana, not exported metrics.

### Observatory

Source: Xray expvars. Ordinary observatory provides alive state and delay;
burstObservatory additionally provides window probe counts and RTT statistics
when HealthPing data is present. RTT statistics require at least one successful
sample. LastSeen/LastTry are not exported.

| Metric | Labels | Values and description |
| --- | --- | --- |
| `xui_observatory_outbound_alive` | `outbound`: outbound tag | Gauge: `1` = alive, `0` = not alive according to Xray. Latest probe state for ordinary observatory; window state for burstObservatory. |
| `xui_observatory_outbound_delay_seconds` | `outbound` | Gauge: nonnegative Xray delay, in seconds. Latest successful probe for ordinary observatory or burst window average. Omitted when Xray reports the outbound as not alive. |
| `xui_observatory_outbound_rtt_mean_seconds` | `outbound` | Gauge: mean RTT in the burst observation window, in seconds. |
| `xui_observatory_outbound_rtt_min_seconds` | `outbound` | Gauge: minimum RTT in the burst observation window, in seconds. |
| `xui_observatory_outbound_rtt_max_seconds` | `outbound` | Gauge: maximum RTT in the burst observation window, in seconds. |
| `xui_observatory_outbound_rtt_deviation_seconds` | `outbound` | Gauge: RTT deviation reported by Xray for the burst observation window, in seconds. |
| `xui_observatory_outbound_probe_samples` | `outbound` | Gauge: total probes in the current burst window, including failures. Not a cumulative counter. |
| `xui_observatory_outbound_probe_failed_samples` | `outbound` | Gauge: failed probes in the current burst window. Not a cumulative counter. |

#### Dashboard Quality score

`Quality` is a dashboard-only heuristic on a 0–100 scale; higher is better.
It is not an exported metric, an Xray leastLoad score, or an input to routing.
All components are calculated per outbound over the selected dashboard time range
(`$__range`) using burstObservatory data.

Inputs:

- `f` = `sum_over_time(probe_failed_samples) / sum_over_time(probe_samples)`.
  This is a failure estimate weighted by the probe counts in each observed window.
- `P50` and `P95` = `quantile_over_time(0.50, rtt_mean_seconds)` and
  `quantile_over_time(0.95, rtt_mean_seconds)`. Both are measured in **seconds**.

The metric names above omit the common `xui_observatory_outbound_` prefix for
readability. `clamp(x, 0, 100)` limits a value to the 0–100 range.

| Component | Formula | Meaning |
| --- | --- | --- |
| Reliability (`R`) | `100 * exp(-10 * f)` | Penalizes probe failures. No failures gives 100; a 1% failure estimate gives about 90.5. |
| RTT stability (`S`) | `100 * exp(-2 * (P95 - P50))` | Penalizes the upper-tail spread of observed mean RTT. A 100 ms P95–P50 gap gives about 81.9. This does not use the exported RTT deviation metric. |
| Latency (`L`) | `clamp(130 - 100 * P50, 0, 100)` | Rewards low median delay: 100 at P50 ≤ 300 ms, 80 at 500 ms, and 0 at P50 ≥ 1.3 s. |

The final score is a **weighted geometric mean**, not an arithmetic average:

```text
Quality = 100 * (R / 100)^0.50 * (S / 100)^0.30 * (L / 100)^0.20
```

The weights are 50% reliability, 30% RTT stability and 20% latency. A low component
pulls the combined score down; a zero latency component makes Quality zero.
The table colors Quality red below 60, orange from 60 to below 80, yellow from
80 to below 90, and green from 90 upward. These weights, constants and thresholds
are dashboard choices, not universal service-quality limits.

Interpretation limits:

- P50/P95 describe sampled **observation-window means**, not individual HTTP
  request latency. The windows overlap, and repeated scrapes may include the
  same probes; `f` is not a failure ratio over unique probes.
- Missing samples are not replaced with zero and missing coverage is not itself
  penalized. RTT statistics exclude windows without successful samples; failures
  affect the Reliability component instead.
- Ordinary observatory lacks the burst inputs needed for this score. Missing
  inputs leave the score absent; a zero total probe denominator makes it
  undefined. Neither case means a score of zero.
- Quality does not measure throughput, application responsiveness or actual user
  traffic. Compare it over the same time range and alongside probe success and RTT.

![Observatory health and dashboard quality scores](docs/images/observatory.jpg)

<details>
<summary>Latency and probe history</summary>

![Observatory latency and burst probes](docs/images/observatory-probes.jpg)

</details>

### Inbounds

Source: Xray expvars traffic statistics. Only counters enabled by Xray traffic
policies are published; user-level counters are excluded.

| Metric | Labels | Values and description |
| --- | --- | --- |
| `xui_inbound_traffic_bytes_total` | `inbound`: inbound tag; `direction`: `uplink` or `downlink` | Counter: cumulative bytes through the inbound. Uplink is client-to-destination traffic; downlink is destination-to-client traffic. Missing directions are omitted, not filled with zero. |

![Inbound throughput and traffic distribution](docs/images/inbounds.jpg)

### Outbounds

Source: Xray expvars traffic statistics. Traffic counters measure transferred
bytes; they do not indicate whether an outbound is currently selected or healthy.

| Metric | Labels | Values and description |
| --- | --- | --- |
| `xui_outbound_traffic_bytes_total` | `outbound`: outbound tag; `direction`: `uplink` or `downlink` | Counter: cumulative bytes through the outbound. Uplink is client-to-destination traffic; downlink is destination-to-client traffic. Only directions enabled by Xray traffic policies are published. |

![Outbound throughput and traffic distribution](docs/images/outbounds.jpg)

### Balancers

Source: Xray gRPC runtime state, with configuration discovery from the panel API.
Selection describes the path a balancer currently prefers, not a log of actual
connections or proof that traffic has traversed that path.

| Metric | Labels | Values and description |
| --- | --- | --- |
| `xui_balancer_outbound_selected` | `balancer`: balancer tag; `role`: `primary`, `fallback` or `override`; `outbound`: tag, or empty for override | Gauge: `1` = selected/active, `0` = confirmed not selected/inactive. The role-specific rules are below. Missing runtime balancers and collection errors produce no selection series. |
| `xui_balancer_outbound_available` | `balancer` | Gauge: `1` = the selected target or fallback resolves to an existing runtime outbound, `0` = no resolvable path. Structural availability only: it does not probe internet access or guarantee channel health. |

#### Selection roles

| Role | Outbound label | Publication and values |
| --- | --- | --- |
| `primary` | Actual runtime candidate/target tag | One series per known selector candidate, plus any returned primary target outside the selector. Selected targets are `1`; others are `0`. A successfully observed empty choice produces confirmed zeros. Subject to strategy support below. |
| `fallback` | Exact configured `fallbackTag` | Published only when fallback is configured and its selection state is known. `1` when primary selection is empty and no override is active; otherwise `0`. A loopback fallback retains its loopback tag, not the final downstream target. |
| `override` | Empty string | A stable series for every successfully read runtime balancer. `1` when override is active, otherwise `0`; its concrete target tag is intentionally not exported. |

An active override sets primary and configured fallback selection to zero.
Empty label values are equivalent to missing labels in Prometheus; identify
override by `role`, not by the presence of `outbound`. The same outbound tag can
appear in primary and fallback roles without a collision.

Availability follows unconditional loopback chains with cycle detection.
Conditional loopback routes require routing context and cause a balancer
collection error. If a rule has both outboundTag and balancerTag, outboundTag
has precedence. A fallback can be active while availability is zero.

#### Strategy support

Selection support is strategy-dependent. **leastLoad is the production-verified
strategy**; do not assume all Xray balancers expose equivalent monitoring data.

| Strategy | Primary selection | Configured fallback activity | Override activity |
| --- | --- | --- | --- |
| leastLoad | Supported | Supported | Supported |
| leastPing | Supported by Xray API and automated tests; not live-verified | Supported | Supported |
| random | Unknown without override; omitted | Unknown without override; omitted | Supported |
| roundRobin | Unknown without override; omitted | Unknown without override; omitted | Supported |

Xray GetBalancerInfo exposes candidate lists, not actual selected targets, for
random and roundRobin. We do not interpret those candidates as active selection
or infer fallback from them. With override active, known primary candidates and
configured fallback report zero, and override reports one. Override activity is
reported even when normal selection cannot be observed.

All four strategies expose structural outbound availability; for random and
roundRobin it describes resolvable candidates/fallback, not proof of their actual
choice or health. Availability is never an internet connectivity guarantee.
For leastLoad/leastPing, principle targets represent current strategy preference,
not a log of routed connections. Runtime routing reads use public Xray gRPC
services and protobuf packages pinned to the v26.9.30 commit. Automated tests
cover selection roles for all four strategies, including empty results and overrides.

![Balancer selection history](docs/images/balancers.jpg)

<details>
<summary>Availability and selected primary target count</summary>

![Balancer availability and selected target count](docs/images/balancer-availability.jpg)

</details>

### Xray

Source: Xray expvars `memstats`. These describe the Xray Go runtime, not the
exporter process. Go-managed memory is not process RSS. Uptime and goroutine
counts are intentionally not exported.

| Metric | Labels | Values and description |
| --- | --- | --- |
| `xui_xray_memory_alloc_bytes` | None | Gauge: bytes occupied by allocated heap objects (`Alloc`). |
| `xui_xray_memory_sys_bytes` | None | Gauge: bytes obtained from the OS by the Go runtime (`Sys`); includes runtime-managed regions, not just live heap objects. |
| `xui_xray_memory_alloc_bytes_total` | None | Counter: cumulative bytes allocated for heap objects, including objects subsequently freed (`TotalAlloc`). |
| `xui_xray_memory_mallocs_total` | None | Counter: cumulative number of heap objects allocated (`Mallocs`). |
| `xui_xray_memory_frees_total` | None | Counter: cumulative number of heap objects freed (`Frees`). |
| `xui_xray_memory_live_objects` | None | Gauge: current number of allocated heap objects (`HeapObjects`). |
| `xui_xray_gc_cycles_total` | None | Counter: completed garbage collection cycles (`NumGC`). |
| `xui_xray_gc_pause_seconds_total` | None | Counter: cumulative GC stop-the-world pause time, converted from nanoseconds to seconds (`PauseTotalNs`). |

![Xray memory and heap allocation metrics](docs/images/xray-process.jpg)

<details>
<summary>Garbage collection</summary>

![Xray garbage collection metrics](docs/images/xray-gc.jpg)

</details>

### Exporter

Source: the exporter itself. A successful Prometheus scrape does not imply that
all upstream sources succeeded: `/metrics` can return HTTP 200 with source
failure gauges and valid metrics from independent sources.

| Metric | Labels | Values and description |
| --- | --- | --- |
| `xui_exporter_scrape_success` | `source`: `api`, `grpc` or `expvars` | Gauge: `1` = all required operations for this source succeeded, `0` = collection or decoding failed. Disabled sources have no series. |
| `xui_exporter_scrape_duration_seconds` | `source`: `api`, `grpc` or `expvars` | Gauge: elapsed wall-clock time of this source's collection work in the current scrape, in seconds. Excludes waiting for other sources; not CPU time. |
| `xui_exporter_build_info` | `version`: embedded build version | Gauge: always `1`; the label identifies the running build. |

The dashboard also displays `up` and `scrape_duration_seconds`, which are created
by Prometheus, not this exporter. API collection runs first; gRPC and expvars
then run concurrently. The duration panel stacks API + max(gRPC, expvars), while
Total is the separate end-to-end duration measured by Prometheus.

![Exporter collection status and duration](docs/images/exporter-stats.jpg)

## Dashboard queries

The role-aware dashboards require an exporter build that includes the `role`
label (v0.2.0 and later). Upgrade the exporter and dashboards together. Selection
queries explicitly exclude pre-upgrade series without a role, so historical data
from the old schema does not double-count targets. Older samples are not migrated.

Selected time (%), instant query:

```promql
100 * avg_over_time(xui_balancer_outbound_selected{balancer=~"$balancer",role=~"primary|fallback|override"}[$__range])
```

Timeline (range query, actual step aligned to scrape interval):

```promql
xui_balancer_outbound_selected{balancer=~"$balancer",role=~"primary|fallback|override"}
```

Use role and outbound in the legend; fallback and override appear alongside
primary targets. Selected target count filters `role="primary"`.

State timeline: fixed color mode, value mappings 0=gray/Not selected and
1=green/Selected, mergeValues=true. Time percentages require regular complete
scrapes. Multiple selected targets may sum above 100%.

Selection state changes in the preceding minute (minute query step, bars aligned before):

```promql
sum by (instance,balancer) (changes(xui_balancer_outbound_selected{role=~"primary|fallback|override"}[1m]))
```

Each 0/1 transition is counted separately across primary, fallback and override.
A target replacement may cause multiple transitions. Minute boundaries and
scrape gaps limit completeness; this is not a connection-switch log.

## Collection timing

API and expvars durations measure their own request/decoding work. gRPC duration
measures only balancer/outbound collection; waiting for API and expvars is
excluded. Prometheus scrape_duration_seconds remains the complete duration.
Operations taking 100 ms or longer emit a warning without secrets.

## Request budget and failures

With N>0 configured balancers: one API request, one HTTP expvars request when
enabled, one ListOutbounds and N GetBalancerInfo calls: N+3 total. For two
balancers with expvars enabled, this is always five calls, including when
expvars fails. No balancers means no gRPC calls. GetSysStats and QueryStats
are never called.

Every metric has exactly one source: API for configuration discovery, gRPC
for runtime outbound inventory and balancers, expvars for traffic, memory/GC
and observation. No caching, source switching or alternate-source fallback collection.
Missing data remains absent. Valid groups survive malformed siblings; decoding
errors set expvars success to zero. Empty or missing optional groups produce
no measurements. Explicitly disabled sources have no status/duration series.

## Panel settings discovery

Each scrape calls POST /panel/api/xray/ with the API token. The exporter reads
the saved xraySetting template and subscriptionOutbounds, excludes disabled
routing rules (preserving the internal API rule), and removes UI-only rule
metadata. This avoids full configuration generation and traffic maintenance
performed by getConfigJson. Runtime inventory and balancer state still come
from gRPC. Settings are not cached and do not prove they were applied to Xray.

## Build and test

```sh
make test
make build
make linux
docker build -t 3x-ui-exporter:dev .
```

The Docker image contains all dependencies and runs unprivileged. Use a read-only
filesystem and drop capabilities (see compose.yaml). For placement alongside
Xray, prefer its container network namespace; no LAN exposure of gRPC is needed.
See VERIFICATION.md for tested scenarios and compatibility scope.

Read-only live checks: `XUI_TEST_EXPVARS_URL=http://xray.example:11111 make test`
and `XUI_TEST_GRPC=host:port XUI_TEST_BALANCER=test-balancer make test`.

## Continuous integration

GitHub Actions runs on pushes to main, pull requests and manual dispatches.
Independent jobs check Go formatting, module files, race tests, go vet,
third-party notices, workflow syntax, dashboard JSON and Git history for secrets.
CI also builds Linux amd64/arm64 binaries and a Docker image, then checks the
packaged version, health endpoint and metrics response with an unavailable panel.
The container runs with a read-only filesystem and no Linux capabilities.
After all checks pass on main, CI uploads Linux amd64/arm64 archives and SHA-256
checksums as an Actions artifact (retained for 14 days). It also publishes the
multi-architecture image `ghcr.io/nike38rus/3x-ui-exporter:edge` and a
`sha-<full-commit-SHA>` tag. Development builds report version
`v0.0.0-dev.<short-commit-SHA>`. Pull requests never publish images or artifacts.

## Releases

CI checks pull requests and main. Pushing a vMAJOR.MINOR.PATCH tag runs tests,
builds static Linux amd64/arm64 archives with SHA-256 checksums, creates a
GitHub release and publishes a multi-architecture GHCR image. A tag such as
v0.2.0 produces ghcr.io/nike38rus/3x-ui-exporter:0.2.0. The first GHCR package
may need its visibility set to public in GitHub package settings.

Build locally with make release VERSION=v0.2.0. Archives include dashboards,
README with its screenshot assets, LICENSE and third-party notices. Build version is embedded via
-X main.version and exposed in xui_exporter_build_info. Release workflows
publish versioned releases when a tag is pushed; local builds do not publish anything.

## License

Original project code is licensed under MIT; see LICENSE. Dependencies retain
their own licenses; see THIRD_PARTY_NOTICES.md and third_party_licenses/.
The linked, unmodified Xray components are MPL-2.0; the notices include exact
source archive links. Container base-image components retain their own licenses.
