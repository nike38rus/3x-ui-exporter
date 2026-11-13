package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestExpvarsResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		status, samples int
		bad             bool
	}{
		{"ordinary", `{"observatory":{"A":{"alive":true,"delay":400,"outbound_tag":"A"},"B":{"delay":99999999}}}`, 200, 3, false},
		{"burst", `{"memstats":{"Alloc":999},"stats":{},"observatory":{"A":{"alive":true,"delay":300,"health_ping":{"all":3,"average":300000000,"min":200000000,"max":400000000,"deviation":10000000}}}}`, 200, 8, false},
		{"missing", `{}`, 200, 0, false},
		{"empty", `{"observatory":{}}`, 200, 0, false},
		{"invalid", `{"observatory":[]}`, 200, 0, true},
		{"null entry", `{"observatory":{"A":null}}`, 200, 0, true},
		{"duplicate tag", `{"observatory":{"A":{"outbound_tag":"X"},"B":{"outbound_tag":"X"}}}`, 200, 0, true},
		{"bad counts", `{"observatory":{"A":{"health_ping":{"all":1,"fail":2}}}}`, 200, 0, true},
		{"bad json", `not json`, 200, 0, true},
		{"http error", `{}`, 503, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/debug/vars" || r.Header.Get("Authorization") != "" {
					t.Error("incorrect path or leaked authorization")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			v, err := (&Expvars{URL: server.URL, Client: server.Client()}).Snapshot(context.Background(), nil)
			var m samples
			if err == nil {
				m, err = v.Observation, v.Err
			}
			if (err != nil) != tc.bad || (!tc.bad && len(m) != tc.samples) {
				t.Fatalf("samples=%d error=%v", len(m), err)
			}
		})
	}
}

func TestExpvarsSourceIsolation(t *testing.T) {
	cfg := configuration(t, `{"observatory":{},"metrics":{"listen":"127.0.0.1:11111"},"routing":{"balancers":[{"tag":"lb","selector":["A"]}]}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	e := &Exporter{Panel: fakePanel{cfg: cfg}, Core: &fakeCore{}, Expvars: &Expvars{URL: server.URL, Client: server.Client()}, Timeout: time.Second}
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{`xui_exporter_scrape_success{source="grpc"} 1`, `xui_exporter_scrape_success{source="api"} 1`, `xui_exporter_scrape_success{source="expvars"} 0`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, "xui_outbound_traffic_bytes_total") {
		t.Error("fabricated traffic from another source")
	}
	if strings.Contains(body, "xui_observatory_outbound_alive{") {
		t.Error("fabricated observation")
	}
	cfg.Metrics.Listen = ""
	w = httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `xui_exporter_scrape_success{source="expvars"} 0`) {
		t.Error("explicit endpoint failure must remain visible")
	}
}

func TestExpvarsDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := (&Expvars{URL: server.URL, Client: server.Client()}).Snapshot(ctx, nil); err == nil {
		t.Fatal("deadline ignored")
	}
}

func TestLiveExpvars(t *testing.T) {
	address := os.Getenv("XUI_TEST_EXPVARS_URL")
	if address == "" {
		t.Skip("XUI_TEST_EXPVARS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	v, err := (&Expvars{URL: address, Client: client}).Snapshot(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := v.Observation, v.Err
	if err != nil {
		t.Fatal(err)
	}
	if !v.TrafficPresent || len(v.Memory) != 8 {
		t.Fatalf("missing live traffic/memory: traffic=%v memory=%d", v.TrafficPresent, len(v.Memory))
	}
	t.Logf("traffic samples: %d; memory samples: %d", len(v.Traffic), len(v.Memory))
	t.Logf("outbounds: %d, observation samples: %d", len(sampleValues(t, m, "xui_observatory_outbound_alive")), len(m))
}

func TestExpvarsHTTPError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		location   string
		wantErr    string
	}{
		{
			name:       "302 redirect with location",
			statusCode: http.StatusFound,
			location:   "http://127.0.0.1:62788/debug/vars",
			wantErr:    "expvars HTTP status 302: redirected to http://127.0.0.1:62788/debug/vars",
		},
		{
			name:       "301 redirect without location",
			statusCode: http.StatusMovedPermanently,
			location:   "",
			wantErr:    "expvars HTTP status 301: redirected",
		},
		{
			name:       "500 internal server error",
			statusCode: http.StatusInternalServerError,
			wantErr:    "expvars HTTP status 500",
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
			err := expvarsHTTPError(resp)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("expvarsHTTPError() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestObservatorySurvivesPartialExpvarsError(t *testing.T) {
	cfg := configuration(t, `{"observatory":{},"metrics":{"listen":"127.0.0.1:11111"},"routing":{"balancers":[{"tag":"lb","selector":["A"],"strategy":{"type":"leastLoad"}}]}}`)
	core := &fakeCore{
		tags:   map[string]bool{"A": true},
		states: map[string]*BalancerState{"lb": {Targets: map[string]bool{"A": true}}},
	}
	for _, tc := range []struct {
		name       string
		body       string
		wantHealth float64
	}{
		{
			name:       "valid observatory with broken stats",
			body:       `{"stats":[],"memstats":{"Alloc":123},"observatory":{"A":{"alive":true,"delay":100,"outbound_tag":"A"}}}`,
			wantHealth: 2,
		},
		{
			name:       "broken observatory with valid stats",
			body:       `{"stats":{"outbound":{"A":{"uplink":10}}},"observatory":{"A":null}}`,
			wantHealth: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			e := &Exporter{
				Panel:   fakePanel{cfg: cfg},
				Core:    core,
				Expvars: &Expvars{URL: server.URL, Client: server.Client()},
			}
			m := e.scrape(context.Background())
			ok := sampleValues(t, m, "xui_exporter_scrape_success")
			if ok["source=expvars;"] != 0 || ok["source=api;"] != 1 || ok["source=grpc;"] != 1 {
				t.Fatalf("unexpected scrape_success: %v", ok)
			}
			health := sampleValues(t, m, "xui_balancer_health")
			if got := health["balancer=lb;"]; got != tc.wantHealth {
				t.Fatalf("xui_balancer_health=%v, want %v", got, tc.wantHealth)
			}
		})
	}
}
