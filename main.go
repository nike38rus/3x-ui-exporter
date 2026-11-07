package main

import (
	"context"
	"crypto/tls"
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
func newHTTPClients(insecureSkipVerify bool) (*http.Client, *http.Client) {
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.Proxy = nil
	baseTransport.MaxIdleConns = 20
	baseTransport.MaxIdleConnsPerHost = 10
	baseTransport.IdleConnTimeout = 90 * time.Second
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	expvarsClient := &http.Client{Transport: baseTransport, CheckRedirect: noRedirect}
	panelClient := expvarsClient
	if insecureSkipVerify {
		panelTransport := baseTransport.Clone()
		panelTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		panelClient = &http.Client{Transport: panelTransport, CheckRedirect: noRedirect}
	}
	return panelClient, expvarsClient
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
	insecureSkipVerify := fs.Bool("xui-insecure-skip-verify", envBool("XUI_INSECURE_SKIP_VERIFY", false), "skip TLS certificate verification for 3x-ui panel requests")
	target := fs.String("xray-api", env("XRAY_API", "127.0.0.1:62789"), "Xray gRPC host:port")
	metricsURL := fs.String("xray-metrics-url", env("XRAY_METRICS_URL", ""), "Xray HTTP metrics base URL; empty discovers metrics.listen through panel API")
	listen := fs.String("listen-address", env("LISTEN_ADDRESS", ":9108"), "metrics host:port")
	timeoutText := fs.String("scrape-timeout", env("SCRAPE_TIMEOUT", "4s"), "total collection deadline")
	logLevel := fs.String("log-level", env("LOG_LEVEL", "info"), "logging level (debug, info, warn, error)")
	logFormat := fs.String("log-format", env("LOG_FORMAT", "text"), "logging format (text, json)")
	collectClientStats := fs.Bool("collect-client-stats", envBool("COLLECT_CLIENT_STATS", false), "export individual client/user traffic counters from Xray")
	basicAuthUser := fs.String("web.basic-auth-username", env("WEB_BASIC_AUTH_USERNAME", ""), "username for exporter basic auth")
	basicAuthPass := fs.String("web.basic-auth-password", env("WEB_BASIC_AUTH_PASSWORD", ""), "password for exporter basic auth (prefer environment variable)")
	basicAuthPassFile := fs.String("web.basic-auth-password-file", env("WEB_BASIC_AUTH_PASSWORD_FILE", ""), "path to file containing password for exporter basic auth")
	bearerToken := fs.String("web.bearer-token", env("WEB_BEARER_TOKEN", ""), "bearer token for exporter authentication (prefer environment variable)")
	bearerTokenFile := fs.String("web.bearer-token-file", env("WEB_BEARER_TOKEN_FILE", ""), "path to file containing bearer token for exporter authentication")
	authHealthz := fs.Bool("web.auth-healthz", envBool("WEB_AUTH_HEALTHZ", false), "require authentication for /healthz endpoint")
	authRealm := fs.String("web.auth-realm", env("WEB_AUTH_REALM", "metrics"), "authentication realm reported in WWW-Authenticate header")
	tlsCertFile := fs.String("web.tls-cert-file", env("WEB_TLS_CERT_FILE", ""), "path to server TLS certificate / chain file")
	tlsKeyFile := fs.String("web.tls-key-file", env("WEB_TLS_KEY_FILE", ""), "path to server TLS private key file")
	tlsClientCAFile := fs.String("web.tls-client-ca-file", env("WEB_TLS_CLIENT_CA_FILE", ""), "path to client CA certificate file for mutual TLS (mTLS)")
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
	panelClient, expvarsClient := newHTTPClients(*insecureSkipVerify)
	grpcCore := newCore(conn)
	exporter := &Exporter{
		Panel:   &Panel{URL: *panelURL, Token: *token, Client: panelClient},
		Core:    grpcCore,
		Expvars: &Expvars{URL: *metricsURL, Client: expvarsClient, CollectClient: *collectClientStats},
		Timeout: timeout,
		Version: version,
	}
	grpcCore.slowThreshold = exporter.slowThreshold()
	authConfig, err := NewServerAuthConfig(*basicAuthUser, *basicAuthPass, *basicAuthPassFile, *bearerToken, *bearerTokenFile, *authHealthz, *authRealm)
	if err != nil {
		return err
	}
	serverTLS := &ServerTLSConfig{
		CertFile:     *tlsCertFile,
		KeyFile:      *tlsKeyFile,
		ClientCAFile: *tlsClientCAFile,
	}
	tlsCfg, err := serverTLS.BuildTLSConfig()
	if err != nil {
		return err
	}
	httpHandler := authConfig.Wrap(exporter.Handler())
	server := &http.Server{
		Addr:              *listen,
		Handler:           httpHandler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      timeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("exporter listening", "address", *listen, "version", version, "tls", tlsCfg != nil, "auth", authConfig.Enabled())
	var serveErr error
	if tlsCfg != nil {
		serveErr = server.ListenAndServeTLS("", "")
	} else {
		serveErr = server.ListenAndServe()
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		return serveErr
	}
	return nil
}
