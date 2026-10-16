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
