package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
