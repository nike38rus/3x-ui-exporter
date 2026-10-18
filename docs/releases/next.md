## Selection roles and dashboard update

- Add `role="primary|fallback|override"` to `xui_balancer_outbound_selected`.
- Publish confirmed zero primary selection instead of gaps when leastLoad/leastPing returns no targets.
- Show configured fallback activity with its real outbound tag and stable override activity with an empty outbound label.
- Update both Grafana dashboard formats: show roles, count primary targets only, and replace the approximate target-replacement count with observed selection-state transitions.
- Document and test strategy-specific support: leastLoad is production-verified, leastPing is API/test-supported, random/roundRobin selection is unknown without override.

### Upgrade

Upgrade the exporter and dashboard together. This changes the selection metric label set. Old role-less history remains in Prometheus but is excluded from the updated selection panels; other panels retain their history. No fallback or override traffic is inferred from connection counters.
