# 3x-ui exporter

Synchronous Prometheus exporter for 3x-ui and its running Xray core. One HTTP
API source discovers configuration; gRPC reads runtime outbounds and balancers.
One HTTP expvars response supplies traffic, memory/GC and observatory results. No polling loop, exporter configuration file, or stale cache.

## Run

```sh
export XUI_URL=http://192.168.10.16:2053
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

## Metrics

* `xui_balancer_outbound_selected{balancer,outbound}`: current leastLoad or
  leastPing target membership; override takes priority. Zeros only for matching
  runtime selector candidates. Selected targets outside selector are retained.
  An empty choice or absent runtime balancer produces no selection series.
* `xui_balancer_outbound_available{balancer}`: a selected target or fallback
  resolves to an existing runtime outbound, not proof of internet access.
  Unconditional loopback chains are resolved with cycle detection. Conditional
  loopback routes cannot be resolved without a routing context and cause a
  balancer collection error. OutboundTag takes precedence over BalancerTag.
* `xui_inbound_traffic_bytes_total{inbound,direction}` and
  `xui_outbound_traffic_bytes_total{outbound,direction}`: counters with `uplink`
  and `downlink`. Read exclusively from expvars stats. Missing traffic stays
  absent; there are no alternate-source requests. User counters are excluded.
* `xui_observatory_outbound_alive{outbound}`: observer alive state.
* `xui_observatory_outbound_delay_seconds{outbound}`: Xray delay (last ordinary
  success or burst average). Failed/sentinel measurements are omitted.
* `xui_observatory_outbound_rtt_{mean,min,max,deviation}_seconds{outbound}`:
  burst statistics, only with HealthPing and successful samples.
* `xui_observatory_outbound_probe_samples{outbound}` and
  `xui_observatory_outbound_probe_failed_samples{outbound}`: window gauges,
  not cumulative counters. LastSeen/LastTry are not exported.
* `xui_xray_*`: eight memory/GC fields from expvars memstats, with unchanged
  names and types. HeapObjects supplies memory_live_objects. Uptime and
  goroutines are intentionally absent. Go runtime memory is not RSS.
* `xui_exporter_scrape_success{source="api|grpc|expvars"}` and
  `xui_exporter_scrape_duration_seconds{source="api|grpc|expvars"}`: source gauges.
* `xui_exporter_build_info{version}`: constant build information.

No missing measurements are filled with zeros. Confirmed zero values and boolean
false states are published. Every scrape constructs fresh metrics. Histograms,
quality scores, and exporter-maintained switch counters are intentionally absent.

### Strategy limitations

Xray GetBalancerInfo exposes candidate lists, not actual targets, for random and
roundRobin. Their `outbound_selected` is omitted unless an override is active.
They still expose structural outbound availability. This is not a health guarantee.
For leastLoad/leastPing, principle targets represent current strategy preference,
not a log of routed connections. Runtime routing reads use
public Xray gRPC services and protobuf packages pinned to the v26.9.30 commit.
One expvars request is shared by all HTTP metric groups. Valid groups survive
malformed siblings; the source success gauge is zero for incomplete decoding.

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
GitHub/GHCR publication and full live exporter verification are separate release steps.

Read-only live checks: `XUI_TEST_EXPVARS_URL=http://192.168.10.16:11111 make test`
and `XUI_TEST_GRPC=host:port XUI_TEST_BALANCER=someTest make test`.

## Dashboard queries

Selected time (%), instant query:

```promql
100 * avg_over_time(xui_balancer_outbound_selected{balancer=~"$balancer"}[$__range])
```

Timeline (range query, actual step aligned to scrape interval):

```promql
xui_balancer_outbound_selected{balancer=~"$balancer"}
```

State timeline: fixed color mode, value mappings 0=gray/Not selected and
1=green/Selected, mergeValues=true. Time percentages require regular complete
scrapes. Multiple selected targets may sum above 100%.

Changes in the preceding minute (minute query step, bars aligned before):

```promql
sum by (instance,balancer) (changes(xui_balancer_outbound_selected[1m])) / 2
```

Division by two assumes constant selected-set size and stable series inventory.
Minute boundaries and scrape gaps limit accuracy; this is observed membership
change, not a connection-switch log.

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
