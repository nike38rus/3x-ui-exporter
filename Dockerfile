FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /exporter .

FROM alpine:3.22
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /exporter /usr/local/bin/3x-ui-exporter
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/licenses/3x-ui-exporter/
COPY third_party_licenses /usr/share/licenses/3x-ui-exporter/third_party_licenses/
LABEL org.opencontainers.image.source="https://github.com/nike38rus/3x-ui-exporter" \
      org.opencontainers.image.licenses="MIT"
USER 65532:65532
EXPOSE 9108
ENTRYPOINT ["/usr/local/bin/3x-ui-exporter"]
