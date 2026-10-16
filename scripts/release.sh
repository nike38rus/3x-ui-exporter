#!/usr/bin/env bash
set -euo pipefail

version="${1:?Pass the release version}"
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then
  printf 'Expected a version such as v0.1.0\n' >&2
  exit 1
fi

mkdir -p dist
for arch in amd64 arm64; do
  archive="3x-ui-exporter_${version}_linux_${arch}"
  stage="$(mktemp -d "${TMPDIR:-/tmp}/xui-release.XXXXXX")"
  trap 'rm -rf "$stage"' EXIT
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags="-s -w -X main.version=$version" -o "$stage/3x-ui-exporter" .
  cp LICENSE THIRD_PARTY_NOTICES.md README.md grafana-3x-ui.*.json "$stage/"
  cp -R third_party_licenses "$stage/"
  tar -czf "dist/${archive}.tar.gz" -C "$stage" .
  rm -rf "$stage"
  trap - EXIT
done
(
  cd dist
  shasum -a 256 "3x-ui-exporter_${version}_linux_amd64.tar.gz" \
    "3x-ui-exporter_${version}_linux_arm64.tar.gz" > "checksums_${version}.txt"
)
