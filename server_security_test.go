package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConstantTimeCompareStrings(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"exact match", "secret-token-123", "secret-token-123", true},
		{"empty strings", "", "", true},
		{"different lengths", "secret", "secret-extended", false},
		{"same length different content", "password123", "password456", false},
		{"prefix match only", "token-abc", "token-xyz", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := constantTimeCompareStrings(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("constantTimeCompareStrings(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestNewServerAuthConfig_Validation(t *testing.T) {
	tmpDir := t.TempDir()

	validTokenFile := filepath.Join(tmpDir, "token.txt")
	if err := os.WriteFile(validTokenFile, []byte("my-secret-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	validPassFile := filepath.Join(tmpDir, "pass.txt")
	if err := os.WriteFile(validPassFile, []byte("my-secret-pass\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	emptyFile := filepath.Join(tmpDir, "empty.txt")
	if err := os.WriteFile(emptyFile, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("both password and password-file error", func(t *testing.T) {
		_, err := NewServerAuthConfig("user", "pass", validPassFile, "", "", false, "")
		if err == nil || !strings.Contains(err.Error(), "both --web.basic-auth-password and --web.basic-auth-password-file") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("both token and token-file error", func(t *testing.T) {
		_, err := NewServerAuthConfig("", "", "", "tok", validTokenFile, false, "")
		if err == nil || !strings.Contains(err.Error(), "both --web.bearer-token and --web.bearer-token-file") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("username without password error", func(t *testing.T) {
		_, err := NewServerAuthConfig("admin", "", "", "", "", false, "")
		if err == nil || !strings.Contains(err.Error(), "basic auth password must be specified") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("password without username error", func(t *testing.T) {
		_, err := NewServerAuthConfig("", "secret", "", "", "", false, "")
		if err == nil || !strings.Contains(err.Error(), "basic auth username must be specified") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("empty password file error", func(t *testing.T) {
		_, err := NewServerAuthConfig("admin", "", emptyFile, "", "", false, "")
		if err == nil || !strings.Contains(err.Error(), "is empty") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing token file error", func(t *testing.T) {
		_, err := NewServerAuthConfig("", "", "", "", filepath.Join(tmpDir, "nonexistent"), false, "")
		if err == nil || !strings.Contains(err.Error(), "failed to read bearer token file") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("auth-healthz without credentials error", func(t *testing.T) {
		_, err := NewServerAuthConfig("", "", "", "", "", true, "")
		if err == nil || !strings.Contains(err.Error(), "--web.auth-healthz") {
			t.Fatalf("expected error when authHealthz is true without credentials, got: %v", err)
		}
	})

	t.Run("valid file-based auth", func(t *testing.T) {
		cfg, err := NewServerAuthConfig("admin", "", validPassFile, "", validTokenFile, true, "custom-realm")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.BasicAuthPassword != "my-secret-pass" {
			t.Errorf("got pass %q, want 'my-secret-pass'", cfg.BasicAuthPassword)
		}
		if cfg.BearerToken != "my-secret-bearer" {
			t.Errorf("got token %q, want 'my-secret-bearer'", cfg.BearerToken)
		}
		if cfg.Realm != "custom-realm" {
			t.Errorf("got realm %q, want 'custom-realm'", cfg.Realm)
		}
		if !cfg.AuthHealthz {
			t.Errorf("expected AuthHealthz to be true")
		}
	})
}

func testEchoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte("ok"))
		case "/metrics":
			_, _ = w.Write([]byte("xui_up 1"))
		default:
			_, _ = w.Write([]byte("landing"))
		}
	})
}

func TestServerAuthConfig_Wrap_NoAuth(t *testing.T) {
	cfg, err := NewServerAuthConfig("", "", "", "", "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled() {
		t.Fatal("expected auth to be disabled")
	}

	handler := cfg.Wrap(testEchoHandler())
	ts := httptest.NewServer(handler)
	defer ts.Close()

	for _, path := range []string{"/healthz", "/metrics", "/"} {
		res, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Errorf("path %s returned %d, want 200", path, res.StatusCode)
		}
		_ = res.Body.Close()
	}
}

func TestServerAuthConfig_Wrap_BasicAuth(t *testing.T) {
	cfg, err := NewServerAuthConfig("monitor", "s3cret", "", "", "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := cfg.Wrap(testEchoHandler())
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// /healthz without auth should succeed because AuthHealthz is false
	res, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("/healthz returned %d, want 200", res.StatusCode)
	}
	_ = res.Body.Close()

	// /metrics without auth should return 401 with WWW-Authenticate: Basic
	res, err = ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("/metrics unauthenticated returned %d, want 401", res.StatusCode)
	}
	wwwAuth := res.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(wwwAuth, "Basic realm=") {
		t.Errorf("WWW-Authenticate header = %q, want Basic realm", wwwAuth)
	}
	_ = res.Body.Close()

	// /metrics with wrong password
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.SetBasicAuth("monitor", "wrongpass")
	res, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("/metrics with wrong password returned %d, want 401", res.StatusCode)
	}
	_ = res.Body.Close()

	// /metrics with valid Basic Auth
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.SetBasicAuth("monitor", "s3cret")
	res, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("/metrics with valid Basic Auth returned %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if !strings.Contains(string(body), "xui_up 1") {
		t.Errorf("unexpected body: %q", string(body))
	}
}

func TestServerAuthConfig_Wrap_BearerToken(t *testing.T) {
	cfg, err := NewServerAuthConfig("", "", "", "secret-bearer-999", "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := cfg.Wrap(testEchoHandler())
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// /metrics without token returns 401 with WWW-Authenticate: Bearer
	res, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("/metrics unauthenticated returned %d, want 401", res.StatusCode)
	}
	wwwAuth := res.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(wwwAuth, "Bearer realm=") {
		t.Errorf("WWW-Authenticate header = %q, want Bearer realm", wwwAuth)
	}
	_ = res.Body.Close()

	// /metrics with wrong token
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	res, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("/metrics with wrong token returned %d, want 401", res.StatusCode)
	}
	_ = res.Body.Close()

	// /metrics with valid token
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret-bearer-999")
	res, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("/metrics with valid Bearer token returned %d, want 200", res.StatusCode)
	}
	_ = res.Body.Close()

	// /metrics with lowercase "bearer "
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "bearer secret-bearer-999")
	res, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("/metrics with lowercase bearer returned %d, want 200", res.StatusCode)
	}
	_ = res.Body.Close()
}

func TestServerAuthConfig_Wrap_BothBasicAndBearer(t *testing.T) {
	cfg, err := NewServerAuthConfig("prom", "prompass", "", "tokenxyz", "", true, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := cfg.Wrap(testEchoHandler())
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Since AuthHealthz is true, /healthz requires auth too
	res, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("/healthz with AuthHealthz=true returned %d, want 401", res.StatusCode)
	}
	authHeaders := res.Header.Values("WWW-Authenticate")
	hasBasic := false
	hasBearer := false
	for _, h := range authHeaders {
		if strings.HasPrefix(h, "Basic realm=") {
			hasBasic = true
		}
		if strings.HasPrefix(h, "Bearer realm=") {
			hasBearer = true
		}
	}
	if !hasBasic || !hasBearer {
		t.Errorf("expected both Basic and Bearer WWW-Authenticate, got: %v", authHeaders)
	}
	_ = res.Body.Close()

	// Basic Auth succeeds
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.SetBasicAuth("prom", "prompass")
	res, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("Basic Auth failed with status %d", res.StatusCode)
	}
	_ = res.Body.Close()

	// Bearer Token succeeds
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer tokenxyz")
	res, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("Bearer Token failed with status %d", res.StatusCode)
	}
	_ = res.Body.Close()
}

// TLS test helper to create self-signed CA and certificates in memory
func createTestCert(t *testing.T, isCA bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, cn string, dns []string, ips []net.IP) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"3x-ui-exporter test"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	if isCA {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	}
	if len(dns) > 0 {
		template.DNSNames = dns
	}
	if len(ips) > 0 {
		template.IPAddresses = ips
	}

	signerCert := template
	signerKey := priv
	if parent != nil && parentKey != nil {
		signerCert = parent
		signerKey = parentKey
	}

	der, err := x509.CreateCertificate(rand.Reader, template, signerCert, &priv.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return parsed, priv, pemBytes
}

func encodeKeyToPEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
}

func TestServerTLSConfig_Validation(t *testing.T) {
	t.Run("empty config returns nil", func(t *testing.T) {
		cfg := &ServerTLSConfig{}
		tlsCfg, err := cfg.BuildTLSConfig()
		if err != nil || tlsCfg != nil {
			t.Fatalf("expected nil, nil, got tlsCfg=%v, err=%v", tlsCfg, err)
		}
	})

	t.Run("cert without key", func(t *testing.T) {
		cfg := &ServerTLSConfig{CertFile: "server.crt"}
		_, err := cfg.BuildTLSConfig()
		if err == nil || !strings.Contains(err.Error(), "--web.tls-key-file") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("key without cert", func(t *testing.T) {
		cfg := &ServerTLSConfig{KeyFile: "server.key"}
		_, err := cfg.BuildTLSConfig()
		if err == nil || !strings.Contains(err.Error(), "--web.tls-cert-file") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("ca without cert/key", func(t *testing.T) {
		cfg := &ServerTLSConfig{ClientCAFile: "ca.crt"}
		_, err := cfg.BuildTLSConfig()
		if err == nil || !strings.Contains(err.Error(), "requires TLS server certificate and key") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestServerTLS_HandshakeAndCiphers(t *testing.T) {
	tmpDir := t.TempDir()

	// Generate CA
	caCert, caKey, caPEM := createTestCert(t, true, nil, nil, "Test Root CA", nil, nil)
	caFile := filepath.Join(tmpDir, "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	// Generate Server Cert
	_, serverKey, serverCertPEM := createTestCert(t, false, caCert, caKey, "127.0.0.1", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	serverKeyPEM := encodeKeyToPEM(t, serverKey)

	certFile := filepath.Join(tmpDir, "server.crt")
	keyFile := filepath.Join(tmpDir, "server.key")
	if err := os.WriteFile(certFile, serverCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, serverKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	serverTLS := &ServerTLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
	}
	tlsCfg, err := serverTLS.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS 1.2 (%d)", tlsCfg.MinVersion, tls.VersionTLS12)
	}
	if len(tlsCfg.CipherSuites) == 0 {
		t.Errorf("expected non-empty modern cipher suites")
	}

	ts := httptest.NewUnstartedServer(testEchoHandler())
	ts.TLS = tlsCfg
	ts.StartTLS()
	defer ts.Close()

	rootPool := x509.NewCertPool()
	rootPool.AppendCertsFromPEM(caPEM)

	// Valid TLS connection with trusted CA
	clientSecure := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: rootPool,
			},
		},
	}
	res, err := clientSecure.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("secure request failed: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	_ = res.Body.Close()

	// Connection with untrusted CA should fail
	clientUntrusted := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: x509.NewCertPool(), // Empty pool
			},
		},
	}
	_, err = clientUntrusted.Get(ts.URL + "/metrics")
	if err == nil {
		t.Fatal("expected certificate verification failure for untrusted client")
	}

	// TLS 1.1 handshake must fail because MinVersion is TLS 1.2
	clientOldTLS := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    rootPool,
				MaxVersion: tls.VersionTLS11,
			},
		},
	}
	_, err = clientOldTLS.Get(ts.URL + "/metrics")
	if err == nil {
		t.Fatal("expected handshake failure when client max version is TLS 1.1")
	}
}

func TestServerMutualTLS_mTLS(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Root CA for Server and Client
	caCert, caKey, caPEM := createTestCert(t, true, nil, nil, "mTLS Root CA", nil, nil)
	caFile := filepath.Join(tmpDir, "client-ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. Server Cert signed by CA
	_, serverKey, serverCertPEM := createTestCert(t, false, caCert, caKey, "127.0.0.1", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	serverCertFile := filepath.Join(tmpDir, "server.crt")
	serverKeyFile := filepath.Join(tmpDir, "server.key")
	if err := os.WriteFile(serverCertFile, serverCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverKeyFile, encodeKeyToPEM(t, serverKey), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Valid Client Cert signed by CA
	_, clientKey, clientCertPEM := createTestCert(t, false, caCert, caKey, "prom-client", nil, nil)
	clientCert, err := tls.X509KeyPair(clientCertPEM, encodeKeyToPEM(t, clientKey))
	if err != nil {
		t.Fatal(err)
	}

	// 4. Untrusted Client Cert signed by another untrusted CA
	untrustedCA, untrustedCAKey, _ := createTestCert(t, true, nil, nil, "Rogue CA", nil, nil)
	_, untrustedClientKey, untrustedClientPEM := createTestCert(t, false, untrustedCA, untrustedCAKey, "rogue-client", nil, nil)
	untrustedClientCert, err := tls.X509KeyPair(untrustedClientPEM, encodeKeyToPEM(t, untrustedClientKey))
	if err != nil {
		t.Fatal(err)
	}

	// Build server TLS config with ClientCAFile
	serverTLS := &ServerTLSConfig{
		CertFile:     serverCertFile,
		KeyFile:      serverKeyFile,
		ClientCAFile: caFile,
	}
	tlsCfg, err := serverTLS.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", tlsCfg.ClientAuth)
	}

	// Auth config with Bearer Token on top of mTLS
	authCfg, err := NewServerAuthConfig("", "", "", "supersecret", "", false, "")
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewUnstartedServer(authCfg.Wrap(testEchoHandler()))
	ts.TLS = tlsCfg
	ts.StartTLS()
	defer ts.Close()

	rootPool := x509.NewCertPool()
	rootPool.AppendCertsFromPEM(caPEM)

	// Case A: Client without client certificate -> Handshake fails
	clientNoCert := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: rootPool,
			},
		},
	}
	_, err = clientNoCert.Get(ts.URL + "/metrics")
	if err == nil {
		t.Fatal("expected handshake failure when client certificate is missing")
	}

	// Case B: Client with untrusted client certificate -> Handshake fails
	clientUntrusted := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      rootPool,
				Certificates: []tls.Certificate{untrustedClientCert},
			},
		},
	}
	_, err = clientUntrusted.Get(ts.URL + "/metrics")
	if err == nil {
		t.Fatal("expected handshake failure with untrusted client certificate")
	}

	// Case C: Client with valid client certificate, but wrong Bearer Token -> 401 Unauthorized
	clientValidMTLS := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      rootPool,
				Certificates: []tls.Certificate{clientCert},
			},
		},
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrongtoken")
	res, err := clientValidMTLS.Do(req)
	if err != nil {
		t.Fatalf("mTLS handshake should succeed: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
	_ = res.Body.Close()

	// Case D: Client with valid client cert AND valid Bearer Token -> 200 OK
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer supersecret")
	res, err = clientValidMTLS.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if !strings.Contains(string(body), "xui_up 1") {
		t.Errorf("unexpected body: %q", string(body))
	}
}

func TestUnauthenticatedResponsesDoNotLeakIdentity(t *testing.T) {
	sensitiveSubstrings := []string{"3x-ui", "3xui", "xray", "x-ui", "panel"}

	assertNoLeak := func(t *testing.T, endpoint string, res *http.Response, body string) {
		t.Helper()
		combined := strings.ToLower(body)
		for k, v := range res.Header {
			combined += " " + strings.ToLower(k) + ": " + strings.ToLower(strings.Join(v, " "))
		}
		for _, s := range sensitiveSubstrings {
			if strings.Contains(combined, s) {
				t.Errorf("endpoint %s leaked sensitive keyword %q in response: %s", endpoint, s, combined)
			}
		}
	}

	exporter := &Exporter{Timeout: time.Second, Version: "dev"}
	baseHandler := exporter.Handler()

	t.Run("auth enabled unauthenticated requests", func(t *testing.T) {
		authCfg, err := NewServerAuthConfig("prom", "pass123", "", "bearer456", "", false, "")
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(authCfg.Wrap(baseHandler))
		defer ts.Close()

		for _, path := range []string{"/", "/metrics", "/unknown", "/healthz"} {
			res, err := ts.Client().Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if path == "/healthz" {
				if res.StatusCode != http.StatusOK {
					t.Errorf("/healthz status = %d, want 200", res.StatusCode)
				}
			} else {
				if res.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s status = %d, want 401", path, res.StatusCode)
				}
				wwwAuth := res.Header.Get("WWW-Authenticate")
				if strings.Contains(strings.ToLower(wwwAuth), "3x-ui") {
					t.Errorf("WWW-Authenticate leaked exporter name: %s", wwwAuth)
				}
			}
			assertNoLeak(t, path, res, string(b))
		}
	})

	t.Run("auth enabled with auth-healthz", func(t *testing.T) {
		authCfg, err := NewServerAuthConfig("prom", "pass123", "", "", "", true, "")
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(authCfg.Wrap(baseHandler))
		defer ts.Close()

		res, err := ts.Client().Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("/healthz status = %d, want 401", res.StatusCode)
		}
		assertNoLeak(t, "/healthz", res, string(b))
	})

	t.Run("open landing page does not mention 3x-ui or panel", func(t *testing.T) {
		ts := httptest.NewServer(baseHandler)
		defer ts.Close()

		res, err := ts.Client().Get(ts.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		assertNoLeak(t, "/", res, string(b))
	})
}

func buildTestReleaseArchive(t *testing.T, ver string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	script := []byte("#!/usr/bin/env bash\nif [[ \"${1:-}\" == \"-version\" ]]; then echo \"" + ver + "\"; fi\n")
	hdr := &tar.Header{
		Name: "3x-ui-exporter",
		Mode: 0o755,
		Size: int64(len(script)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(script); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

func TestInstallScriptSecurityAndContracts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	const (
		relVer      = "v1.4.0"
		archiveName = "3x-ui-exporter_v1.4.0_linux_amd64.tar.gz"
		secretToken = "super-secret-bearer-token-do-not-print"
	)

	archiveBytes, validSum := buildTestReleaseArchive(t, relVer)
	var serveCorrupt atomic.Bool

	releaseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + relVer + "/" + archiveName:
			if serveCorrupt.Load() {
				_, _ = w.Write([]byte("corrupted-archive-bytes"))
				return
			}
			_, _ = w.Write(archiveBytes)
		case "/" + relVer + "/checksums_" + relVer + ".txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", validSum, archiveName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseSrv.Close()

	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not available")
	}

	var metricsHealthy atomic.Bool
	metricsHealthy.Store(true)
	var metricsEmpty atomic.Bool
	metricsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+secretToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if metricsEmpty.Load() {
			_, _ = w.Write([]byte("# empty\n"))
			return
		}
		if metricsHealthy.Load() {
			_, _ = w.Write([]byte("xui_exporter_scrape_success{source=\"api\"} 1\nxui_exporter_scrape_success{source=\"expvars\"} 1\nxui_exporter_scrape_success{source=\"grpc\"} 1\n"))
			return
		}
		_, _ = w.Write([]byte("xui_exporter_scrape_success{source=\"api\"} 0\nxui_exporter_scrape_success{source=\"expvars\"} 1\nxui_exporter_scrape_success{source=\"grpc\"} 1\n"))
	}))
	defer metricsSrv.Close()

	listenAddr := strings.TrimPrefix(metricsSrv.URL, "https://")

	setupStubs := func(t *testing.T, serviceActive bool) (string, string, string, string, []string) {
		t.Helper()
		dir := t.TempDir()
		fakeBin := filepath.Join(dir, "fakebin")
		if err := os.MkdirAll(fakeBin, 0o755); err != nil {
			t.Fatal(err)
		}
		activeExit := "0"
		if !serviceActive {
			activeExit = "3"
		}
		systemctlScript := "#!/usr/bin/env bash\nif [[ \"${1:-}\" == \"is-active\" ]]; then exit " + activeExit + "; fi\nexit 0\n"
		if err := os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte(systemctlScript), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fakeBin, "journalctl"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fakeBin, "sleep"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		curlLogPath := filepath.Join(dir, "curl_args.log")
		curlWrapper := fmt.Sprintf("#!/usr/bin/env bash\nprintf '%%s\\n' \"$*\" >> %q\nexec %q \"$@\"\n", curlLogPath, realCurl)
		if err := os.WriteFile(filepath.Join(fakeBin, "curl"), []byte(curlWrapper), 0o755); err != nil {
			t.Fatal(err)
		}

		binDir := filepath.Join(dir, "custom-bin")
		cfgPath := filepath.Join(dir, "custom-etc", "exporter.env")
		unitPath := filepath.Join(dir, "custom-systemd", "3x-ui-exporter.service")
		certPath := filepath.Join(dir, "fullchain.pem")
		keyPath := filepath.Join(dir, "privkey.pem")
		_ = os.WriteFile(certPath, []byte("dummy-cert"), 0o600)
		_ = os.WriteFile(keyPath, []byte("dummy-key"), 0o600)

		env := append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"NON_INTERACTIVE=1",
			"SKIP_ROOT_CHECK=1",
			"SKIP_SYSTEMD_CHECK=1",
			"TARGET_OS=Linux",
			"TARGET_ARCH=amd64",
			"RELEASE_BASE_URL="+releaseSrv.URL,
			"GITHUB_LATEST_URL="+releaseSrv.URL+"/missing-latest",
			"GITHUB_API_LATEST_URL="+releaseSrv.URL+"/missing-api-latest",
			"BIN_DIR="+binDir,
			"CONFIG_PATH="+cfgPath,
			"SYSTEMD_PATH="+unitPath,
			"TLS_CERT="+certPath,
			"TLS_KEY="+keyPath,
			"BEARER_TOKEN="+secretToken,
			"WEB_AUTH_HEALTHZ=true",
		)
		return binDir, cfgPath, unitPath, curlLogPath, env
	}

	t.Run("custom paths, checksum verification, and no bearer token leak in output or curl args", func(t *testing.T) {
		serveCorrupt.Store(false)
		metricsHealthy.Store(true)
		metricsEmpty.Store(false)
		binDir, cfgPath, unitPath, curlLogPath, env := setupStubs(t, true)

		cmd := exec.Command("bash", "./install.sh", "install",
			"-v", relVer,
			"-t", "panel-admin-token",
			"-u", "http://127.0.0.1:2053",
			"-l", listenAddr,
			"-B", secretToken,
		)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		output := string(out)
		if err != nil {
			t.Fatalf("install failed: %v\nOutput:\n%s", err, output)
		}

		// Verify bearer token is not printed to stdout/stderr
		if strings.Contains(output, secretToken) {
			t.Errorf("install output leaked bearer token %q:\n%s", secretToken, output)
		}

		// Verify custom paths in systemd unit
		unitBytes, err := os.ReadFile(unitPath)
		if err != nil {
			t.Fatalf("failed to read systemd unit: %v", err)
		}
		unitContent := string(unitBytes)
		expectedExec := "ExecStart=" + filepath.Join(binDir, "3x-ui-exporter")
		expectedEnv := "EnvironmentFile=" + cfgPath
		if !strings.Contains(unitContent, expectedExec) {
			t.Errorf("systemd unit missing %q:\n%s", expectedExec, unitContent)
		}
		if !strings.Contains(unitContent, expectedEnv) {
			t.Errorf("systemd unit missing %q:\n%s", expectedEnv, unitContent)
		}

		// Verify status and update also do not leak the bearer token
		for _, subcmd := range [][]string{{"status"}, {"update", "-v", relVer}} {
			c := exec.Command("bash", append([]string{"./install.sh"}, subcmd...)...)
			c.Env = env
			subOut, subErr := c.CombinedOutput()
			if subErr != nil {
				t.Fatalf("%v failed: %v\nOutput:\n%s", subcmd, subErr, string(subOut))
			}
			if strings.Contains(string(subOut), secretToken) {
				t.Errorf("%v output leaked bearer token:\n%s", subcmd, string(subOut))
			}
		}

		// Verify bearer token was never passed in curl command-line arguments
		curlArgsBytes, err := os.ReadFile(curlLogPath)
		if err != nil {
			t.Fatalf("failed to read curl args log: %v", err)
		}
		if strings.Contains(string(curlArgsBytes), secretToken) {
			t.Errorf("curl command-line arguments leaked bearer token:\n%s", string(curlArgsBytes))
		}
	})

	t.Run("checksum mismatch aborts installation", func(t *testing.T) {
		serveCorrupt.Store(true)
		defer serveCorrupt.Store(false)
		binDir, _, _, _, env := setupStubs(t, true)

		cmd := exec.Command("bash", "./install.sh", "install",
			"-v", relVer,
			"-t", "panel-admin-token",
			"-l", listenAddr,
		)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected install to fail on checksum mismatch, but succeeded:\n%s", string(out))
		}
		if _, statErr := os.Stat(filepath.Join(binDir, "3x-ui-exporter")); !os.IsNotExist(statErr) {
			t.Errorf("corrupted binary should not have been installed")
		}
	})

	t.Run("inactive service returns non-zero exit code on install and update", func(t *testing.T) {
		serveCorrupt.Store(false)
		_, _, _, _, env := setupStubs(t, false)

		cmd := exec.Command("bash", "./install.sh", "install",
			"-v", relVer,
			"-t", "panel-admin-token",
			"-l", listenAddr,
		)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("expected install to fail when service is inactive:\n%s", string(out))
		}

		updateCmd := exec.Command("bash", "./install.sh", "update", "-v", relVer)
		updateCmd.Env = env
		if out, err := updateCmd.CombinedOutput(); err == nil {
			t.Fatalf("expected update to fail when service is inactive:\n%s", string(out))
		}
	})

	t.Run("self-test failure returns non-zero exit code on missing or failed sources", func(t *testing.T) {
		serveCorrupt.Store(false)
		metricsEmpty.Store(true)
		_, _, _, _, env := setupStubs(t, true)

		cmd := exec.Command("bash", "./install.sh", "install",
			"-v", relVer,
			"-t", "panel-admin-token",
			"-l", listenAddr,
			"-B", secretToken,
		)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("expected install to fail when self-test metrics are missing:\n%s", string(out))
		}

		metricsEmpty.Store(false)
		metricsHealthy.Store(false)
		defer metricsHealthy.Store(true)
		_, _, _, _, envUnhealthy := setupStubs(t, true)
		cmdUnhealthy := exec.Command("bash", "./install.sh", "install",
			"-v", relVer,
			"-t", "panel-admin-token",
			"-l", listenAddr,
			"-B", secretToken,
		)
		cmdUnhealthy.Env = envUnhealthy
		if out, err := cmdUnhealthy.CombinedOutput(); err == nil {
			t.Fatalf("expected install to fail when a scrape source reports 0:\n%s", string(out))
		}
	})

	t.Run("version discovery failure aborts without fallback to outdated version", func(t *testing.T) {
		serveCorrupt.Store(false)
		metricsHealthy.Store(true)
		metricsEmpty.Store(false)
		_, _, _, _, env := setupStubs(t, true)

		cmd := exec.Command("bash", "./install.sh", "install",
			"-t", "panel-admin-token",
			"-l", listenAddr,
		)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("expected install without -v to fail when version discovery fails:\n%s", string(out))
		}
	})

	t.Run("config file created with 0600 permissions before chmod and incomplete TLS fails fast", func(t *testing.T) {
		serveCorrupt.Store(false)
		metricsHealthy.Store(true)
		metricsEmpty.Store(false)
		_, cfgPath, _, _, env := setupStubs(t, true)

		// Replace chmod with a no-op stub to prove umask 077 created CONFIG_PATH as 0600 initially
		var fakeBin string
		for _, e := range env {
			if strings.HasPrefix(e, "PATH=") {
				fakeBin = strings.Split(strings.TrimPrefix(e, "PATH="), string(os.PathListSeparator))[0]
			}
		}
		if err := os.WriteFile(filepath.Join(fakeBin, "chmod"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command("bash", "./install.sh", "install",
			"-v", relVer,
			"-t", "panel-admin-token",
			"-l", listenAddr,
			"-B", secretToken,
		)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install failed: %v\n%s", err, string(out))
		}
		info, err := os.Stat(cfgPath)
		if err != nil {
			t.Fatalf("failed to stat config file: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("expected config file to be created with 0600 permissions via umask, got %04o", perm)
		}

		// Incomplete TLS (cert without key) in non-interactive mode must fail immediately
		_, _, _, _, envNoKey := setupStubs(t, true)
		var filteredEnv []string
		for _, e := range envNoKey {
			if !strings.HasPrefix(e, "TLS_KEY=") && !strings.HasPrefix(e, "WEB_TLS_KEY_FILE=") {
				filteredEnv = append(filteredEnv, e)
			}
		}
		cmdNoKey := exec.Command("bash", "./install.sh", "install",
			"-v", relVer,
			"-t", "panel-admin-token",
			"-l", listenAddr,
		)
		cmdNoKey.Env = filteredEnv
		if out, err := cmdNoKey.CombinedOutput(); err == nil {
			t.Fatalf("expected install with cert but no key to fail in non-interactive mode:\n%s", string(out))
		}
	})

	t.Run("update rolls back to previous binary on failure and config-tls prints prometheus snippet", func(t *testing.T) {
		serveCorrupt.Store(false)
		metricsHealthy.Store(true)
		metricsEmpty.Store(false)
		binDir, _, _, _, env := setupStubs(t, true)

		// Install initial version
		cmdInstall := exec.Command("bash", "./install.sh", "install",
			"-v", relVer,
			"-t", "panel-admin-token",
			"-l", listenAddr,
			"-B", secretToken,
		)
		cmdInstall.Env = env
		if out, err := cmdInstall.CombinedOutput(); err != nil {
			t.Fatalf("initial install failed: %v\n%s", err, string(out))
		}

		// Verify config-tls outputs prometheus.yml snippet without leaking secret token
		cmdTLS := exec.Command("bash", "./install.sh", "config-tls", "-B", secretToken)
		cmdTLS.Env = env
		tlsOutBytes, err := cmdTLS.CombinedOutput()
		tlsOut := string(tlsOutBytes)
		if err != nil {
			t.Fatalf("config-tls failed: %v\n%s", err, tlsOut)
		}
		if !strings.Contains(tlsOut, "job_name: xui-exporter") || !strings.Contains(tlsOut, "prometheus.yml") || !strings.Contains(tlsOut, "ca_file:") {
			t.Errorf("expected config-tls to output prometheus.yml snippet with ca_file hint, got:\n%s", tlsOut)
		}
		if strings.Contains(tlsOut, "insecure_skip_verify") {
			t.Errorf("config-tls snippet must not recommend insecure_skip_verify:\n%s", tlsOut)
		}
		if strings.Contains(tlsOut, secretToken) {
			t.Errorf("config-tls output leaked bearer token:\n%s", tlsOut)
		}

		// Replace installed binary with a marker v1.3.0 binary before running a failing update
		binPath := filepath.Join(binDir, "3x-ui-exporter")
		oldScript := []byte("#!/usr/bin/env bash\nif [[ \"${1:-}\" == \"-version\" ]]; then echo \"v1.3.0\"; fi\n")
		if err := os.WriteFile(binPath, oldScript, 0o755); err != nil {
			t.Fatal(err)
		}

		// 1. Make daemon-reload fail inside create_or_update_systemd (before restart)
		var fakeBin string
		for _, e := range env {
			if strings.HasPrefix(e, "PATH=") {
				fakeBin = strings.Split(strings.TrimPrefix(e, "PATH="), string(os.PathListSeparator))[0]
			}
		}
		failReloadScript := "#!/usr/bin/env bash\nif [[ \"${1:-}\" == \"daemon-reload\" ]]; then exit 1; fi\nexit 0\n"
		if err := os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte(failReloadScript), 0o755); err != nil {
			t.Fatal(err)
		}
		cmdUpdateReloadFail := exec.Command("bash", "./install.sh", "update", "-v", relVer)
		cmdUpdateReloadFail.Env = env
		if out, err := cmdUpdateReloadFail.CombinedOutput(); err == nil {
			t.Fatalf("expected update to fail when daemon-reload fails:\n%s", string(out))
		}
		verOutReload, err := exec.Command(binPath, "-version").CombinedOutput()
		if err != nil || strings.TrimSpace(string(verOutReload)) != "v1.3.0" {
			t.Fatalf("expected rollback to v1.3.0 when daemon-reload fails, got %q (err=%v)", string(verOutReload), err)
		}
		// Restore normal systemctl stub
		if err := os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		// 2. Make self-test fail during update (scrape_success=0)
		metricsHealthy.Store(false)
		defer metricsHealthy.Store(true)

		cmdUpdate := exec.Command("bash", "./install.sh", "update", "-v", relVer)
		cmdUpdate.Env = env
		if out, err := cmdUpdate.CombinedOutput(); err == nil {
			t.Fatalf("expected update to fail when self-test fails:\n%s", string(out))
		}

		// Verify binary was rolled back to v1.3.0
		verOut, err := exec.Command(binPath, "-version").CombinedOutput()
		if err != nil {
			t.Fatalf("failed to run rolled-back binary: %v", err)
		}
		if got := strings.TrimSpace(string(verOut)); got != "v1.3.0" {
			t.Errorf("expected rolled-back binary version to be v1.3.0, got %q", got)
		}
	})
}

func TestNewHTTPClientsIsolatesInsecureSkipVerifyToPanel(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	panelClient, expvarsClient := newHTTPClients(true)
	if panelClient == expvarsClient {
		t.Fatal("expected distinct HTTP clients when insecureSkipVerify is true")
	}

	// panelClient should accept the self-signed certificate
	res, err := panelClient.Get(ts.URL)
	if err != nil {
		t.Fatalf("expected panelClient with insecureSkipVerify=true to succeed against self-signed TLS server, got: %v", err)
	}
	_ = res.Body.Close()

	// expvarsClient must still verify certificates and reject the self-signed certificate
	if _, err := expvarsClient.Get(ts.URL); err == nil {
		t.Fatal("expected expvarsClient to reject self-signed TLS certificate even when panel insecureSkipVerify=true")
	}
}
