FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS base-files
RUN echo "nonroot:x:65532:65532:nonroot:/:/sbin/nologin" > /etc/passwd.minimal

FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS binary-source
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /exporter .

FROM scratch AS binary-prebuilt
ARG TARGETARCH
COPY bin/3x-ui-exporter-linux-${TARGETARCH} /exporter

ARG BINARY_SOURCE=source
FROM binary-${BINARY_SOURCE} AS binary-selected

FROM scratch
COPY --from=base-files /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=base-files /etc/passwd.minimal /etc/passwd
COPY --from=binary-selected /exporter /usr/local/bin/3x-ui-exporter
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/licenses/3x-ui-exporter/
COPY third_party_licenses /usr/share/licenses/3x-ui-exporter/third_party_licenses/
LABEL org.opencontainers.image.source="https://github.com/nike38rus/3x-ui-exporter" \
      org.opencontainers.image.licenses="MIT"
USER 65532:65532
EXPOSE 9108
ENTRYPOINT ["/usr/local/bin/3x-ui-exporter"]
