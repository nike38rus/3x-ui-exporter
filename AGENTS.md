# Instructions for AI Agents & Contributors

This document defines the architecture, metric contracts, and contribution guidelines for `3x-ui-exporter`.

Before changing metrics, dashboards, or documentation, inspect the current implementation and repository state. Conversation history provides context but must not override the current verified contract.

## 1. Project Scope

`3x-ui-exporter` is a lightweight, single-binary Prometheus exporter for 3x-ui and its Xray core. The goal is a coherent, ready-to-use monitoring solution with useful metrics and matching Grafana dashboards.

- Use Go.
- Follow Xray terminology: inbound, outbound, balancer, Observatory, and burstObservatory.
- Use the `xui_` namespace for application metrics.
- Keep collection read-only. Do not modify configuration, restart services, or trigger maintenance operations during a scrape.
- Do not add management features, packet inspection, access-log processing, or external connectivity tests.
- Per-client metrics and quotas are outside the current scope unless explicitly requested. Aggregate online-client counts are supported.
- Geodata monitoring and upstream update checks are outside the current scope unless explicitly requested.

## 2. Collection Architecture

### Fresh data on every scrape

Each Prometheus scrape must collect current upstream data.

- Do not cache collected values or configuration between scrapes.
- Do not introduce background polling, periodic collection loops, or persistent storage.
- Do not return results from an earlier scrape when the current collection fails.
- Reusable HTTP transports, keep-alive connections, and persistent gRPC connections are encouraged. Transport reuse is not a data cache.
- Execute independent requests concurrently.
- Respect request dependencies and a shared collection deadline controlled by `SCRAPE_TIMEOUT`.
- Propagate cancellation to upstream requests.

### Three data sources

The exporter uses three sources:

- `api`: 3x-ui HTTP API.
- `grpc`: Xray gRPC API.
- `expvars`: Xray metrics endpoint.

Assign each metric family a defined source and document the mapping in `README.md`.

- Do not introduce alternative-source fallbacks for the same measurements.
- If the assigned source fails, omit the affected measurements.
- Do not read local configuration files as an additional source of truth.
- Read current configuration through the panel API.
- Additional requests to an existing source are acceptable when their cost and operational value are justified.

### Metrics endpoint discovery

- An explicit `XRAY_METRICS_URL` takes precedence over discovery.
- When it is empty, discover the endpoint from the current configuration's `metrics.listen` on each scrape.
- Normalize wildcard bind addresses to suitable loopback addresses for connecting.
- Do not guess ports or reuse a previously discovered address.
- Failure to discover an endpoint must not prevent unrelated sources from being collected.

### Request cost

Minimize upstream work rather than optimizing only exporter-side execution.

- Prefer bulk methods over one request per entity.
- Avoid increasing request counts with the number of balancers or outbounds when a suitable bulk method exists.
- Evaluate what an endpoint does internally, including database work, file parsing, and external requests.
- Do not call endpoints that trigger downloads, upstream release checks, connectivity tests, or other external activity during collection.
- Do not introduce expensive validation or file parsing merely to retrieve metadata.
- Measure relevant changes. Do not impose hardware-specific latency or RSS targets as universal correctness requirements.

## 3. Source Success Contract

`xui_exporter_scrape_success` uses the `source` label with values `api`, `grpc`, and `expvars`. It reports source access and response-contract validity.

Publish a diagnostic result for all three sources on every completed scrape.

- `1`: the required requests to that source succeeded, and their responses satisfy the exporter's expected contract.
- `0`: source access, a required request, decoding, or contract validation failed.

Source success does not mean that every optional metric exists or that monitored components are healthy.

The following are valid configurations, not collection failures:

- No balancers exist.
- Observatory is not configured.
- Traffic accounting is disabled for some or all directions.
- A successful response contains no entities.
- Optional measurements are absent as permitted by the source contract.

Additional rules:

- Always call gRPC `ListOutbounds`, even when there are no balancers or panel configuration is unavailable.
- Only issue configuration-dependent RPCs when their required inputs are known.
- An API failure must not be reported as a gRPC failure.
- A failure in one endpoint must not discard usable results from successful independent endpoints.
- If any required request within a source fails, its success gauge is `0`, even when some measurements from that source remain available.
- Exporter unavailability is represented by Prometheus `up`; do not synthesize source results for scrapes that never completed.
- Log collection errors with enough operation context to diagnose them, without exposing secrets.

## 4. Missing Data and Metric Semantics

Never replace unknown or unavailable measurements with zero.

- Omit a metric when its value cannot be determined.
- Publish zero when zero is the actual measured state.
- A confirmed failed probe may legitimately produce `alive=0`.
- A failed request must not produce a fabricated healthy or unhealthy component state.
- Do not retain disappeared entities through cached series.
- Do not confuse configuration-dependent absence with collection failure.

### Balancers and observation

- Keep configured membership, runtime selection, and observed health distinct.
- Balancer outbound availability is not an independent end-to-end Internet connectivity test.
- Preserve the documented primary, fallback, and override selection model.
- Do not assume that leastLoad selection semantics apply to leastPing, random, or round-robin strategies.
- Document strategy limitations explicitly.
- Support the common Observatory measurements where available, and publish burstObservatory-specific measurements only when supplied.
- Missing burst-specific measurements in a regular Observatory response are not failures.
- Do not add RTT or deviation histogram buckets without an explicit requirement.

## 5. Metric Names, Labels, and Dependencies

Follow Prometheus naming and instrumentation conventions.

- Use base units such as bytes and seconds.
- Use `_total` for cumulative counters.
- Use labels for dimensions of the same measurement, such as `direction="uplink"` and `direction="downlink"`.
- Use separate metric names for different concepts.
- Keep names and label meanings consistent with the monitored entities.
- Do not add redundant aliases for compatibility without an explicit requirement.
- Do not expose secrets, client identifiers, or unbounded values as labels.
- Let Prometheus attach target labels such as `job` and `instance`.

Existing functional namespaces include:

- `xui_inbound_*`
- `xui_outbound_*`
- `xui_balancer_*`
- `xui_observatory_*`
- `xui_xray_*`
- `xui_panel_*`
- `xui_exporter_*`

Service-wide metrics may use names directly under `xui_`, including `xui_open_sockets` and `xui_online_clients`. Do not introduce artificial namespace segments solely to fit this list.

For socket protocol labels, use `protocol="tcp"` and `protocol="udp"`.

Prefer the Go standard library and existing dependencies. Add dependencies only when justified, review their licenses, and keep `THIRD_PARTY_NOTICES.md` synchronized using the repository's license tooling.

## 6. Grafana Dashboards

### Operational meaning

Each panel must answer a clear monitoring or diagnostic question.

- Keep Overview compact and useful regardless of entity count.
- Preserve detailed panels for investigation.
- Avoid duplicating identical state and version panels across sections.
- Add drill-down links where they lead to useful additional detail.
- Verify links against the supported Grafana version rather than assuming URL behavior.

The Overview traffic panel intentionally shows:

- Client download throughput (inbound downlink rate) above the X axis.
- Client upload throughput (inbound uplink rate) below the X axis.

Use Grafana's `negative-Y` rendering transform for upload so tooltip values retain their original positive meaning.

### Query correctness

- Keep PromQL minimal.
- Do not add compatibility branches for historical metric or label formats unless requested.
- Preserve missing-data semantics; do not use zero-filling to disguise unavailable measurements.
- For `$__rate_interval`, set the query Min step to match the intended scrape interval. Panel Min interval alone is insufficient.
- Allow query resolution to grow for long time ranges.
- Preserve deliberate aggregation windows and matching bar alignment.
- Do not sum parallel collection durations as though they were sequential.
- Do not stack Prometheus scrape duration on top of exporter collection durations.
- Keep tooltip ordering intentional.

### Two dashboard formats

Maintain both files together:

- `grafana-3x-ui.v2.json`
- `grafana-3x-ui.classic.json`

For dashboard changes:

- Validate both JSON files.
- Verify equivalent PromQL and panel semantics across formats.
- Preserve row-scoped variables and their filters.
- Avoid unrelated changes to formulas, descriptions, colors, layout, and other presentation settings.
- Review web exports for unintended changes before committing.
- Do not claim Grafana-version compatibility that has not been verified.

## 7. Documentation and Privacy

Update documentation when behavior changes.

- Document each metric's labels, values, meaning, and source.
- Keep limitations and supported strategy behavior explicit.
- Explain derived metrics and dashboard calculations where their meaning is not obvious.
- Keep example configuration, release notes, dashboards, and screenshots consistent with the implemented version.
- Preserve the project's stated as-is support and maintenance policy.

Do not commit actual private infrastructure details or credentials.

- Sanitize screenshots, fixtures, logs, examples, and documentation.
- Replace real tokens, account identifiers, hostnames, addresses, and entity names with clearly fictional examples.
- Generic loopback or private-network addresses are acceptable in examples.
- Inspect the entire screenshot, including navigation bars, variables, legends, and tooltips.
- Never print secrets merely to inspect configuration.

## 8. Verification and Publication

Use checks appropriate to the change. Inspect the repository's current tooling before choosing commands.

### Go changes

- Format changed Go files with `gofmt`.
- Run `make test`, including the race detector and required static checks.
- Verify relevant build targets.
- Add tests for meaningful behavior, especially source contracts, missing data, and failure isolation.

### Dependency changes

- Run `make licenses`.
- Review changes to dependency files and third-party notices.

### Dashboard changes

- Validate JSON syntax.
- Compare expressions and behavior between both formats.
- Validate changed PromQL against Prometheus when available.
- Inspect rendered panels when visualization behavior changes.

### Before a release

- Run the full repository checks and supported builds.
- Verify release assets, image tags, examples, documentation, and dashboards.
- Confirm that the working tree and release contents contain only intended changes.
- Report any checks that could not be completed.

Keep local preparation, committing, pushing, deployment, and releasing within the user's authorized scope. Do not infer publication or deployment permission from a request to implement a change. Do not request authorization again when it has already been given for the action.

Do not rewrite history, move published release tags, or replace published artifacts without explicit authorization.

The project follows SemVer and has reached `v1.0.0`. Incompatible changes to public metric names, types, labels, or semantics require a major version increment and documented migration instructions.
