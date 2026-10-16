FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /exporter .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /exporter /usr/local/bin/3x-ui-exporter
USER 65532:65532
EXPOSE 9108
ENTRYPOINT ["/usr/local/bin/3x-ui-exporter"]
