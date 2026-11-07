package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPanelSettingsNormalization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obj := json.RawMessage(`{"xraySetting":{"routing":{"balancers":[{"tag":"lb","selector":["sub"],"fallbackTag":"bridge"}],"rules":[{"enabled":false,"inboundTag":["loop"],"outboundTag":"wrong"},{"enabled":true,"comment":"UI metadata","inboundTag":["loop"],"outboundTag":"sub-A"},{"enabled":false,"inboundTag":["api"],"outboundTag":"api"}]},"outbounds":[{"tag":"bridge","protocol":"loopback","settings":{"inboundTag":"loop"}}]},"subscriptionOutbounds":[{"tag":"sub-A","protocol":"vless"}]}`)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": string(obj)})
	}))
	defer server.Close()
	panel := Panel{URL: server.URL, Token: "test", Client: server.Client()}
	cfg, err := panel.Configuration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Outbounds) != 2 || len(cfg.Routing.Rules) != 2 {
		t.Fatalf("unexpected normalized configuration: %+v", cfg)
	}
	for _, rule := range cfg.Routing.Rules {
		if rule["enabled"] != nil || rule["comment"] != nil {
			t.Fatal("UI metadata retained")
		}
	}
	ok, err := resolveAvailable("lb", cfg, map[string]*BalancerState{"lb": {Targets: map[string]bool{}}}, map[string]bool{"bridge": true, "sub-A": true}, map[string]bool{})
	if err != nil || !ok {
		t.Fatalf("subscription fallback: %v %v", ok, err)
	}
}

func TestPanelRejectsMissingTemplate(t *testing.T) {
	for _, obj := range []string{`{}`, `{"xraySetting":null}`, `{"xraySetting":[]}`, `{"xraySetting":"{}"}`} {
		t.Run(obj, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": json.RawMessage(obj)})
			}))
			defer server.Close()
			panel := Panel{URL: server.URL, Client: server.Client()}
			if _, err := panel.Configuration(context.Background()); err == nil {
				t.Fatal("missing/invalid template accepted")
			}
		})
	}
}

func TestPanelHTTPError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		location   string
		wantErr    string
	}{
		{
			name:       "301 redirect to https",
			statusCode: http.StatusMovedPermanently,
			location:   "https://127.0.0.1:2053/panel/api/xray/",
			wantErr:    "panel HTTP status 301: redirected to https://127.0.0.1:2053/panel/api/xray/",
		},
		{
			name:       "302 redirect to login",
			statusCode: http.StatusFound,
			location:   "/login",
			wantErr:    "panel HTTP status 302: redirected to /login",
		},
		{
			name:       "307 redirect without location",
			statusCode: http.StatusTemporaryRedirect,
			location:   "",
			wantErr:    "panel HTTP status 307: redirected",
		},
		{
			name:       "308 permanent redirect",
			statusCode: http.StatusPermanentRedirect,
			location:   "https://example.com/panel/api/xray/",
			wantErr:    "panel HTTP status 308: redirected to https://example.com/panel/api/xray/",
		},
		{
			name:       "401 unauthorized",
			statusCode: http.StatusUnauthorized,
			wantErr:    "panel HTTP status 401: unauthorized (check XUI_API_TOKEN)",
		},
		{
			name:       "403 forbidden",
			statusCode: http.StatusForbidden,
			wantErr:    "panel HTTP status 403: Administrator scope required",
		},
		{
			name:       "500 internal server error",
			statusCode: http.StatusInternalServerError,
			wantErr:    "panel HTTP status 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tt.statusCode,
				Header:     make(http.Header),
			}
			if tt.location != "" {
				resp.Header.Set("Location", tt.location)
			}
			err := panelHTTPError(resp)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("panelHTTPError() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
