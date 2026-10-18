package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	obs "github.com/xtls/xray-core/app/observatory"
	hc "github.com/xtls/xray-core/app/proxyman/command"
	rc "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fakeCore struct {
	states map[string]*BalancerState
	tags   map[string]bool
	fail   bool
}

func (f *fakeCore) Outbounds(context.Context) (map[string]bool, error) {
	if f.fail {
		return nil, errors.New("failed")
	}
	return f.tags, nil
}
func (f *fakeCore) Balancer(_ context.Context, tag string) (*BalancerState, error) {
	return f.states[tag], nil
}

type fakePanel struct {
	cfg *Config
	err error
}

func (f fakePanel) Configuration(context.Context) (*Config, error) { return f.cfg, f.err }
func configuration(t *testing.T, s string) *Config {
	t.Helper()
	var c Config
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}
func sampleValues(t *testing.T, m samples, name string) map[string]float64 {
	t.Helper()
	r := prometheus.NewRegistry()
	r.MustRegister(snapshot{m})
	families, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.Metric {
			key := ""
			for _, l := range m.Label {
				key += l.GetName() + "=" + l.GetValue() + ";"
			}
			if m.Gauge != nil {
				out[key] = m.Gauge.GetValue()
			} else {
				out[key] = m.Counter.GetValue()
			}
		}
	}
	return out
}
func TestSelectorsAndOverride(t *testing.T) {
	cfg := configuration(t, `{"routing":{"balancers":[{"tag":"lb","selector":["A","AB"],"strategy":{"type":"leastLoad"}}]}}`)
	f := &fakeCore{tags: map[string]bool{"A": true, "AB": true, "direct": true}, states: map[string]*BalancerState{"lb": {Targets: map[string]bool{"AB": true}}}}
	e := &Exporter{Core: f}
	m, err := e.balancerMetrics(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	vals := sampleValues(t, m, "xui_balancer_outbound_selected")
	if len(vals) != 3 || vals["balancer=lb;outbound=AB;role=primary;"] != 1 {
		t.Fatal(vals)
	}
	f.states["lb"] = &BalancerState{Targets: map[string]bool{"direct": true}, Override: true}
	m, err = e.balancerMetrics(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v := sampleValues(t, m, "xui_balancer_outbound_selected"); len(v) != 3 || v["balancer=lb;outbound=;role=override;"] != 1 || v["balancer=lb;outbound=AB;role=primary;"] != 0 {
		t.Fatal(v)
	}
}
func TestFallbackChainsAndCycles(t *testing.T) {
	cfg := configuration(t, `{"routing":{"balancers":[{"tag":"main","selector":["A"],"fallbackTag":"loop"},{"tag":"reserve","selector":["B"]}],"rules":[{"inboundTag":["loop-in"],"balancerTag":"reserve"}]},"outbounds":[{"tag":"loop","protocol":"loopback","settings":{"inboundTag":"loop-in"}}]}`)
	states := map[string]*BalancerState{"main": {Targets: map[string]bool{}}, "reserve": {Targets: map[string]bool{"B": true}}}
	tags := map[string]bool{"loop": true, "B": true}
	ok, err := resolveAvailable("main", cfg, states, tags, map[string]bool{})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	delete(tags, "B")
	ok, err = resolveAvailable("main", cfg, states, tags, map[string]bool{})
	if err != nil || ok {
		t.Fatal(ok, err)
	}
	states["reserve"].Targets = map[string]bool{"loop": true}
	ok, err = resolveAvailable("main", cfg, states, tags, map[string]bool{})
	if err != nil || ok {
		t.Fatal(ok, err)
	}
	cfg.Routing.Rules[0]["domain"] = json.RawMessage(`["example.com"]`)
	_, err = resolveAvailable("main", cfg, states, tags, map[string]bool{})
	if err == nil {
		t.Fatal("conditional route accepted")
	}
}
func TestObservationOptionalFieldsAndSentinels(t *testing.T) {
	m, err := observationMetrics(&obs.ObservationResult{Status: []*obs.OutboundStatus{{OutboundTag: "ordinary", Alive: true, Delay: 400}, {OutboundTag: "dead", Delay: 99999999}, {OutboundTag: "burst", Alive: true, Delay: 300, HealthPing: &obs.HealthPingMeasurementResult{All: 3, Fail: 1, Average: 300000000, Deviation: 10000000}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sampleValues(t, m, "xui_observatory_outbound_alive")) != 3 {
		t.Fatal("alive")
	}
	if len(sampleValues(t, m, "xui_observatory_outbound_delay_seconds")) != 2 {
		t.Fatal("sentinel exposed")
	}
	if len(sampleValues(t, m, "xui_observatory_outbound_rtt_mean_seconds")) != 1 {
		t.Fatal("optional fields")
	}
}
func TestTrafficZeroAndMissing(t *testing.T) {
	m, err := trafficMetrics([]*TrafficCounter{{Name: "outbound>>>original-tag>>>traffic>>>uplink", Value: 0}, {Name: "user>>>private>>>traffic>>>downlink", Value: 4}})
	if err != nil {
		t.Fatal(err)
	}
	v := sampleValues(t, m, "xui_outbound_traffic_bytes_total")
	if len(v) != 1 || v["direction=uplink;outbound=original-tag;"] != 0 {
		t.Fatal(v)
	}
	m, err = trafficMetrics(nil)
	if err != nil || len(m) != 0 {
		t.Fatal("empty response")
	}
}
func TestIndependentSourcesAndConcurrentScrapes(t *testing.T) {
	cfg := configuration(t, `{"metrics":{"listen":"localhost:11111"},"routing":{"balancers":[{"tag":"lb","selector":["A"]}]}}`)
	f := &fakeCore{fail: true}
	e := &Exporter{Panel: fakePanel{cfg: cfg}, Core: f, Expvars: staticExpvars{}, Timeout: time.Second, Version: "test"}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			e.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, `xui_exporter_scrape_success{source="grpc"} 0`) {
				t.Error(body)
			}
		}()
	}
	wg.Wait()
	e.Panel = fakePanel{err: errors.New("panel failure")}
	f.fail = false
	m := e.scrape(context.Background())
	if len(sampleValues(t, m, "xui_outbound_traffic_bytes_total")) != 1 {
		t.Fatal("API failure hid traffic")
	}
}
func TestPanelAuthenticationAndStringObject(t *testing.T) {
	for _, encoded := range []bool{false, true} {
		t.Run(map[bool]string{true: "string", false: "object"}[encoded], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" || r.Method != http.MethodPost || r.URL.Path != "/panel/api/xray/" {
					t.Error("request")
				}
				var obj any = map[string]any{"xraySetting": map[string]any{"routing": map[string]any{"balancers": []any{}}}}
				if encoded {
					obj = `{"xraySetting":{"routing":{"balancers":[]}}}`
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": obj})
			}))
			defer server.Close()
			p := &Panel{URL: server.URL, Token: "test-token", Client: server.Client()}
			if _, err := p.Configuration(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type rpcServer struct {
	hc.UnimplementedHandlerServiceServer
	rc.UnimplementedRoutingServiceServer
}

func (rpcServer) ListOutbounds(context.Context, *hc.ListOutboundsRequest) (*hc.ListOutboundsResponse, error) {
	return &hc.ListOutboundsResponse{Outbounds: []*core.OutboundHandlerConfig{{Tag: "A"}}}, nil
}
func (rpcServer) GetBalancerInfo(_ context.Context, r *rc.GetBalancerInfoRequest) (*rc.GetBalancerInfoResponse, error) {
	if r.Tag == "absent" {
		return nil, status.Error(codes.Unknown, "app/router: cannot find tag")
	}
	return &rc.GetBalancerInfoResponse{Balancer: &rc.BalancerMsg{PrincipleTarget: &rc.PrincipleTargetInfo{Tag: []string{"A"}}}}, nil
}
func TestRealGRPCBindings(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	svc := rpcServer{}
	hc.RegisterHandlerServiceServer(server, svc)
	rc.RegisterRoutingServiceServer(server, svc)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := newCore(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if v, e := c.Outbounds(ctx); e != nil || !v["A"] {
		t.Fatal(v, e)
	}
	if v, e := c.Balancer(ctx, "lb"); e != nil || !v.Targets["A"] {
		t.Fatal(v, e)
	}
	if v, e := c.Balancer(ctx, "absent"); e != nil || v != nil {
		t.Fatal(v, e)
	}
}
func TestHTTPTimeoutAndNoRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	p := &Panel{URL: server.URL, Token: "secret", Client: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Configuration(ctx); err == nil {
		t.Fatal("deadline ignored")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com", http.StatusFound)
	}))
	defer redirect.Close()
	p.URL = redirect.URL
	p.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if _, err := p.Configuration(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
}
func TestHandlerRoutes(t *testing.T) {
	e := &Exporter{}
	for _, path := range []string{"/", "/healthz", "/missing"} {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		_, _ = io.ReadAll(w.Result().Body)
		expected := 200
		if path == "/missing" {
			expected = 404
		}
		if w.Code != expected {
			t.Fatal(w.Code)
		}
	}
}

func TestSelectionRoles(t *testing.T) {
	for _, tc := range []struct {
		name, strategy              string
		state                       *BalancerState
		primary, fallback, override float64
		known                       bool
	}{
		{"primary", "leastLoad", &BalancerState{Targets: map[string]bool{"A": true}}, 1, 0, 0, true},
		{"fallback", "leastLoad", &BalancerState{Targets: map[string]bool{}}, 0, 1, 0, true},
		{"override", "leastLoad", &BalancerState{Targets: map[string]bool{"A": true}, Override: true}, 0, 0, 1, true},
		{"leastping primary", "leastPing", &BalancerState{Targets: map[string]bool{"A": true}}, 1, 0, 0, true},
		{"leastping override", "leastPing", &BalancerState{Targets: map[string]bool{"outside-selector": true}, Override: true}, 0, 0, 1, true},
		{"random override", "random", &BalancerState{Targets: map[string]bool{"outside-selector": true}, Override: true}, 0, 0, 1, true},
		{"roundrobin candidates", "roundRobin", &BalancerState{Targets: map[string]bool{"A": true}}, 0, 0, 0, false},
		{"roundrobin empty", "roundRobin", &BalancerState{Targets: map[string]bool{}}, 0, 0, 0, false},
		{"roundrobin override", "roundRobin", &BalancerState{Targets: map[string]bool{"outside-selector": true}, Override: true}, 0, 0, 1, true},
		{"random empty", "random", &BalancerState{Targets: map[string]bool{}}, 0, 0, 0, false},
		{"leastping empty", "leastPing", &BalancerState{Targets: map[string]bool{}}, 0, 1, 0, true},
		{"random unknown", "random", &BalancerState{Targets: map[string]bool{"A": true}}, 0, 0, 0, false},
		{"absent", "leastLoad", nil, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configuration(t, `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"A","strategy":{"type":"`+tc.strategy+`"}}]}}`)
			e := &Exporter{Core: &fakeCore{tags: map[string]bool{"A": true}, states: map[string]*BalancerState{"lb": tc.state}}}
			m, err := e.balancerMetrics(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			v := sampleValues(t, m, "xui_balancer_outbound_selected")
			if tc.state == nil {
				if len(v) != 0 {
					t.Fatal(v)
				}
				return
			}
			if got, exists := v["balancer=lb;outbound=;role=override;"]; !exists || got != tc.override {
				t.Fatal(v)
			}
			if !tc.known {
				if len(v) != 1 {
					t.Fatal(v)
				}
				return
			}
			if len(v) != 3 || v["balancer=lb;outbound=A;role=primary;"] != tc.primary || v["balancer=lb;outbound=A;role=fallback;"] != tc.fallback {
				t.Fatal(v)
			}
		})
	}
}

func TestSelectionRolesCollectionFailure(t *testing.T) {
	cfg := configuration(t, `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"reserve","strategy":{"type":"leastLoad"}}]}}`)
	e := &Exporter{Core: &fakeCore{fail: true}}
	m, err := e.balancerMetrics(context.Background(), cfg)
	if err == nil || len(m) != 0 {
		t.Fatalf("failed collection must not synthesize selection states: %v %v", m, err)
	}
}
