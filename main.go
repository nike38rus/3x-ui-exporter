package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var version = "dev"

func env(key, fallback string) string {
	if s, ok := os.LookupEnv(key); ok {
		return s
	}
	return fallback
}
func envBool(key string, fallback bool) bool {
	if s, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "1", "t", "true", "yes", "on":
			return true
		case "0", "f", "false", "no", "off":
			return false
		}
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		slog.Error("exporter stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	fs := flag.NewFlagSet("3x-ui-exporter", flag.ContinueOnError)
	panelURL := fs.String("xui-url", env("XUI_URL", ""), "3x-ui base URL, including optional base path")
	token := fs.String("xui-api-token", env("XUI_API_TOKEN", ""), "3x-ui bearer token (prefer environment variable)")
	target := fs.String("xray-api", env("XRAY_API", "127.0.0.1:62789"), "Xray gRPC host:port")
	metricsURL := fs.String("xray-metrics-url", env("XRAY_METRICS_URL", ""), "Xray HTTP metrics base URL; empty discovers metrics.listen through panel API")
	listen := fs.String("listen-address", env("LISTEN_ADDRESS", ":9108"), "metrics host:port")
	timeoutText := fs.String("scrape-timeout", env("SCRAPE_TIMEOUT", "4s"), "total collection deadline")
	logLevel := fs.String("log-level", env("LOG_LEVEL", "info"), "logging level (debug, info, warn, error)")
	logFormat := fs.String("log-format", env("LOG_FORMAT", "text"), "logging format (text, json)")
	collectClientStats := fs.Bool("collect-client-stats", envBool("COLLECT_CLIENT_STATS", false), "export individual client/user traffic counters from Xray")
	showVersion := fs.Bool("version", false, "print exporter version")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	var level slog.Level
	switch strings.ToLower(*logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error")
	}
	handlerOpts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(*logFormat) {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, handlerOpts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, handlerOpts)
	default:
		return fmt.Errorf("LOG_FORMAT must be text or json")
	}
	slog.SetDefault(slog.New(handler))
	if !validPanelURL(*panelURL) {
		return fmt.Errorf("XUI_URL must be an HTTP(S) URL without credentials, query or fragment")
	}
	if *metricsURL != "" && !validPanelURL(*metricsURL) {
		return fmt.Errorf("XRAY_METRICS_URL must be an HTTP(S) base URL without credentials, query or fragment")
	}
	if *token == "" || strings.ContainsAny(*token, "\r\n") {
		return fmt.Errorf("XUI_API_TOKEN must be a nonempty single-line token")
	}
	if _, _, err := net.SplitHostPort(*target); err != nil {
		return fmt.Errorf("XRAY_API must be host:port")
	}
	timeout, err := time.ParseDuration(*timeoutText)
	if err != nil || timeout <= 0 {
		return fmt.Errorf("SCRAPE_TIMEOUT must be a positive duration")
	}
	conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("invalid gRPC endpoint")
	}
	defer conn.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = 20
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	exporter := &Exporter{
		Panel:   &Panel{URL: *panelURL, Token: *token, Client: client},
		Core:    newCore(conn),
		Expvars: &Expvars{URL: *metricsURL, Client: client, CollectClient: *collectClientStats},
		Timeout: timeout,
		Version: version,
	}
	server := &http.Server{Addr: *listen, Handler: exporter.Handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: timeout + 5*time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("exporter listening", "address", *listen, "version", version)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
