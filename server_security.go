package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// ServerAuthConfig configures inbound HTTP authentication for the exporter.
type ServerAuthConfig struct {
	BasicAuthUsername string
	BasicAuthPassword string
	BearerToken       string
	AuthHealthz       bool
	Realm             string
}

// ServerTLSConfig configures inbound TLS and mTLS for the exporter HTTP server.
type ServerTLSConfig struct {
	CertFile     string
	KeyFile      string
	ClientCAFile string
}

func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if len(s) == 0 {
		return "", fmt.Errorf("secret file %q is empty", path)
	}
	return s, nil
}

// NewServerAuthConfig validates and constructs ServerAuthConfig from flags/env values.
func NewServerAuthConfig(username, password, passwordFile, token, tokenFile string, authHealthz bool, realm string) (*ServerAuthConfig, error) {
	if password != "" && passwordFile != "" {
		return nil, errors.New("cannot specify both --web.basic-auth-password and --web.basic-auth-password-file")
	}
	if token != "" && tokenFile != "" {
		return nil, errors.New("cannot specify both --web.bearer-token and --web.bearer-token-file")
	}

	finalPass := password
	if passwordFile != "" {
		p, err := readSecretFile(passwordFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read basic auth password file: %w", err)
		}
		finalPass = p
	}

	finalToken := token
	if tokenFile != "" {
		t, err := readSecretFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read bearer token file: %w", err)
		}
		finalToken = t
	}

	if username != "" && finalPass == "" {
		return nil, errors.New("basic auth password must be specified when username is provided")
	}
	if finalPass != "" && username == "" {
		return nil, errors.New("basic auth username must be specified when password is provided")
	}

	if realm == "" {
		realm = "metrics"
	}

	return &ServerAuthConfig{
		BasicAuthUsername: username,
		BasicAuthPassword: finalPass,
		BearerToken:       finalToken,
		AuthHealthz:       authHealthz,
		Realm:             realm,
	}, nil
}

// Enabled returns true if any authentication method is configured.
func (a *ServerAuthConfig) Enabled() bool {
	if a == nil {
		return false
	}
	return (a.BasicAuthUsername != "" && a.BasicAuthPassword != "") || a.BearerToken != ""
}

func constantTimeCompareStrings(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// Authenticate verifies the request credentials in constant time.
func (a *ServerAuthConfig) Authenticate(r *http.Request) bool {
	if !a.Enabled() {
		return true
	}

	// Basic Auth verification
	if a.BasicAuthUsername != "" && a.BasicAuthPassword != "" {
		u, p, ok := r.BasicAuth()
		if ok {
			userMatch := constantTimeCompareStrings(u, a.BasicAuthUsername)
			passMatch := constantTimeCompareStrings(p, a.BasicAuthPassword)
			if userMatch && passMatch {
				return true
			}
		}
	}

	// Bearer Token verification
	if a.BearerToken != "" {
		auth := r.Header.Get("Authorization")
		if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
			token := strings.TrimSpace(auth[7:])
			if constantTimeCompareStrings(token, a.BearerToken) {
				return true
			}
		}
	}

	return false
}

func (a *ServerAuthConfig) writeUnauthorized(w http.ResponseWriter) {
	realm := a.Realm
	if realm == "" {
		realm = "metrics"
	}
	if a.BasicAuthUsername != "" && a.BasicAuthPassword != "" {
		w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, realm))
	}
	if a.BearerToken != "" {
		w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, realm))
	}
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

// Wrap wraps an http.Handler with authentication checks.
func (a *ServerAuthConfig) Wrap(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.AuthHealthz && r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if !a.Authenticate(r) {
			a.writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BuildTLSConfig constructs a secure tls.Config or returns nil if TLS is not enabled.
func (t *ServerTLSConfig) BuildTLSConfig() (*tls.Config, error) {
	if t == nil || (t.CertFile == "" && t.KeyFile == "" && t.ClientCAFile == "") {
		return nil, nil
	}

	if t.CertFile == "" && t.KeyFile != "" {
		return nil, errors.New("--web.tls-cert-file (WEB_TLS_CERT_FILE) must be specified when --web.tls-key-file is provided")
	}
	if t.CertFile != "" && t.KeyFile == "" {
		return nil, errors.New("--web.tls-key-file (WEB_TLS_KEY_FILE) must be specified when --web.tls-cert-file is provided")
	}
	if t.ClientCAFile != "" && t.CertFile == "" {
		return nil, errors.New("--web.tls-client-ca-file requires TLS server certificate and key")
	}

	cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS server certificate/key: %w", err)
	}

	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
	}

	if t.ClientCAFile != "" {
		caPEM, err := os.ReadFile(t.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read client CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("failed to parse client CA certificates from %s", t.ClientCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsCfg, nil
}
