# Contributing

This is a personal, as-is project, not an actively supported community project.
Issues will not be read, and pull requests are unlikely to be reviewed or merged.
If you need changes, fork the repository and maintain your own version.

The notes below are for working on your own fork.

Use the Go toolchain specified in go.mod. Run `gofmt` on changed Go files and
`make test build linux` before publishing changes. Add tests for changed
collection semantics, failures and missing data. Use fake/local services unless
explicitly opting into live checks.

Do not log API tokens, full panel settings or user traffic data. Every metric
has one authoritative source; do not introduce stale caches, alternate-source
fallbacks or synthetic zeros for absent data without revisiting the metric contract.

After dependency changes, run `make licenses` (Python 3 required), review the
updated third-party notices, and include them in the same change. Both Grafana
JSON files are maintained exports; keep their queries and behavior aligned.
