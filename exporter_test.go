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
func (f fakePanel) Status(context.Context) (*PanelStatus, error)   { return &PanelStatus{}, nil }
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
func TestBalancerCandidateMatching(t *testing.T) {
	t.Run("exact_tags", func(t *testing.T) {
		cfg := configuration(t, `{"routing":{"balancers":[{"tag":"lb1","selector":["is-ch-awg","nl-ams-wg","de-fra-vless"],"strategy":{"type":"leastLoad"}}]}}`)
		f := &fakeCore{
			tags: map[string]bool{
				"is-ch-awg":    true,
				"nl-ams-wg":    true,
				"de-fra-vless": true,
				"direct":       true,
			},
			states: map[string]*BalancerState{
				"lb1": {Targets: map[string]bool{"nl-ams-wg": true}},
			},
		}
		e := &Exporter{Core: f}
		m, err := e.balancerMetrics(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		vals := sampleValues(t, m, "xui_balancer_outbound_selected")
		if len(vals) != 4 {
			t.Fatalf("expected 4 metric samples, got %d: %v", len(vals), vals)
		}
		if vals["balancer=lb1;outbound=;role=override;"] != 0 {
			t.Errorf("override expected 0, got %v", vals["balancer=lb1;outbound=;role=override;"])
		}
		if vals["balancer=lb1;outbound=nl-ams-wg;role=primary;"] != 1 {
			t.Errorf("winner expected 1, got %v", vals["balancer=lb1;outbound=nl-ams-wg;role=primary;"])
		}
		if vals["balancer=lb1;outbound=is-ch-awg;role=primary;"] != 0 {
			t.Errorf("candidate is-ch-awg expected 0, got %v", vals["balancer=lb1;outbound=is-ch-awg;role=primary;"])
		}
		if vals["balancer=lb1;outbound=de-fra-vless;role=primary;"] != 0 {
			t.Errorf("candidate de-fra-vless expected 0, got %v", vals["balancer=lb1;outbound=de-fra-vless;role=primary;"])
		}
		if _, exists := vals["balancer=lb1;outbound=direct;role=primary;"]; exists {
			t.Errorf("direct should not be a candidate")
		}
	})

	t.Run("selectors_with_whitespace_and_empty", func(t *testing.T) {
		cfg := configuration(t, `{"routing":{"balancers":[{"tag":"lb2","selector":[" tag1 ","   ","","\ttag2\t"],"strategy":{"type":"leastPing"}}]}}`)
		f := &fakeCore{
			tags: map[string]bool{
				"tag1":   true,
				"tag2":   true,
				"other":  true,
				"direct": true,
			},
			states: map[string]*BalancerState{
				"lb2": {Targets: map[string]bool{"tag1": true}},
			},
		}
		e := &Exporter{Core: f}
		m, err := e.balancerMetrics(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		vals := sampleValues(t, m, "xui_balancer_outbound_selected")
		if len(vals) != 3 {
			t.Fatalf("expected 3 metric samples, got %d: %v", len(vals), vals)
		}
		if vals["balancer=lb2;outbound=tag1;role=primary;"] != 1 {
			t.Errorf("winner expected 1, got %v", vals["balancer=lb2;outbound=tag1;role=primary;"])
		}
		if vals["balancer=lb2;outbound=tag2;role=primary;"] != 0 {
			t.Errorf("candidate tag2 expected 0, got %v", vals["balancer=lb2;outbound=tag2;role=primary;"])
		}
		if _, exists := vals["balancer=lb2;outbound=other;role=primary;"]; exists {
			t.Errorf("empty selector should not match 'other'")
		}
		if _, exists := vals["balancer=lb2;outbound=direct;role=primary;"]; exists {
			t.Errorf("empty selector should not match 'direct'")
		}
	})

	t.Run("mixed_prefixes_and_exact_with_fallback", func(t *testing.T) {
		cfg := configuration(t, `{"routing":{"balancers":[{"tag":"lb3","selector":["pref-"," special "],"fallbackTag":"fb","strategy":{"type":"leastLoad"}}]}}`)
		f := &fakeCore{
			tags: map[string]bool{
				"pref-a":  true,
				"pref-b":  true,
				"special": true,
				"fb":      true,
				"unused":  true,
			},
			states: map[string]*BalancerState{
				"lb3": {Targets: map[string]bool{"pref-b": true}},
			},
		}
		e := &Exporter{Core: f}
		m, err := e.balancerMetrics(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		vals := sampleValues(t, m, "xui_balancer_outbound_selected")
		if len(vals) != 5 {
			t.Fatalf("expected 5 metric samples, got %d: %v", len(vals), vals)
		}
		if vals["balancer=lb3;outbound=;role=override;"] != 0 {
			t.Errorf("override expected 0, got %v", vals["balancer=lb3;outbound=;role=override;"])
		}
		if vals["balancer=lb3;outbound=fb;role=fallback;"] != 0 {
			t.Errorf("fallback expected 0, got %v", vals["balancer=lb3;outbound=fb;role=fallback;"])
		}
		if vals["balancer=lb3;outbound=pref-b;role=primary;"] != 1 {
			t.Errorf("winner pref-b expected 1, got %v", vals["balancer=lb3;outbound=pref-b;role=primary;"])
		}
		if vals["balancer=lb3;outbound=pref-a;role=primary;"] != 0 {
			t.Errorf("pref-a expected 0, got %v", vals["balancer=lb3;outbound=pref-a;role=primary;"])
		}
		if vals["balancer=lb3;outbound=special;role=primary;"] != 0 {
			t.Errorf("special expected 0, got %v", vals["balancer=lb3;outbound=special;role=primary;"])
		}
		if _, exists := vals["balancer=lb3;outbound=unused;role=primary;"]; exists {
			t.Errorf("unused should not be matched")
		}
	})
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
	m, _, err := observationMetrics(&obs.ObservationResult{Status: []*obs.OutboundStatus{{OutboundTag: "ordinary", Alive: true, Delay: 400}, {OutboundTag: "dead", Delay: 99999999}, {OutboundTag: "burst", Alive: true, Delay: 300, HealthPing: &obs.HealthPingMeasurementResult{All: 3, Fail: 1, Average: 300000000, Deviation: 10000000}}}})
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

type emptyTargetRPCServer struct {
	rpcServer
}

func (emptyTargetRPCServer) ListOutbounds(context.Context, *hc.ListOutboundsRequest) (*hc.ListOutboundsResponse, error) {
	return &hc.ListOutboundsResponse{Outbounds: []*core.OutboundHandlerConfig{{Tag: "A"}, {Tag: "blocked"}}}, nil
}

func (emptyTargetRPCServer) GetBalancerInfo(context.Context, *rc.GetBalancerInfoRequest) (*rc.GetBalancerInfoResponse, error) {
	return &rc.GetBalancerInfoResponse{Balancer: &rc.BalancerMsg{PrincipleTarget: &rc.PrincipleTargetInfo{Tag: []string{""}}}}, nil
}

func TestGRPCBalancerEmptyTargetFallback(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	svc := emptyTargetRPCServer{}
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

	state, err := c.Balancer(ctx, "lb")
	if err != nil {
		t.Fatalf("expected no error on empty principle target tag, got: %v", err)
	}
	if state == nil || len(state.Targets) != 0 || state.Override {
		t.Fatalf("expected empty targets and override=false, got: %+v", state)
	}

	cfg := configuration(t, `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"blocked","strategy":{"type":"leastPing"}}]},"outbounds":[{"tag":"A","protocol":"vless"},{"tag":"blocked","protocol":"blackhole"}]}`)
	exporter := &Exporter{Core: c}
	metrics, err := exporter.balancerMetrics(ctx, cfg)
	if err != nil {
		t.Fatalf("unexpected balancerMetrics error: %v", err)
	}
	stateVals := sampleValues(t, metrics, "xui_balancer_state")
	if stateVals["balancer=lb;"] != 1 {
		t.Fatalf("expected balancer_state 1 (fallback), got %v", stateVals["balancer=lb;"])
	}
	selectedVals := sampleValues(t, metrics, "xui_balancer_outbound_selected")
	if selectedVals["balancer=lb;outbound=blocked;role=fallback;"] != 1 {
		t.Fatalf("expected role=fallback selected for blocked, got %v", selectedVals)
	}
	if selectedVals["balancer=lb;outbound=A;role=primary;"] != 0 {
		t.Fatalf("expected role=primary unselected for A, got %v", selectedVals)
	}
	healthVals := sampleValues(t, metrics, "xui_balancer_health")
	if healthVals["balancer=lb;"] != 0 {
		t.Fatalf("expected balancer_health 0 (DOWN) for blackhole fallback, got %v", healthVals["balancer=lb;"])
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

type partialFailCore struct {
	tags    map[string]bool
	states  map[string]*BalancerState
	failTag string
}

func (c *partialFailCore) Outbounds(ctx context.Context) (map[string]bool, error) {
	return c.tags, nil
}
func (c *partialFailCore) Balancer(ctx context.Context, tag string) (*BalancerState, error) {
	if tag == c.failTag {
		return nil, errors.New("balancer RPC failed")
	}
	return c.states[tag], nil
}

func TestPartialBalancerFailurePreservesSuccessfulMeasurements(t *testing.T) {
	cfg := configuration(t, `{"routing":{"balancers":[
		{"tag":"lb-ok","selector":["A"],"strategy":{"type":"leastLoad"}},
		{"tag":"lb-err","selector":["B"],"strategy":{"type":"leastLoad"}}
	]}}`)
	core := &partialFailCore{
		tags:    map[string]bool{"A": true, "B": true},
		states:  map[string]*BalancerState{"lb-ok": {Targets: map[string]bool{"A": true}}},
		failTag: "lb-err",
	}
	e := &Exporter{Core: core}
	m, err := e.balancerMetrics(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error from failed balancer RPC")
	}
	avail := sampleValues(t, m, "xui_balancer_outbound_available")
	if len(avail) != 1 || avail["balancer=lb-ok;"] != 1 {
		t.Fatalf("expected lb-ok available metric to be preserved, got: %v", avail)
	}
	sel := sampleValues(t, m, "xui_balancer_outbound_selected")
	if len(sel) == 0 || sel["balancer=lb-ok;outbound=A;role=primary;"] != 1 {
		t.Fatalf("expected lb-ok selected metrics to be preserved, got: %v", sel)
	}
	for k := range avail {
		if strings.Contains(k, "lb-err") {
			t.Fatalf("failed balancer must not emit available metric: %v", avail)
		}
	}
	for k := range sel {
		if strings.Contains(k, "lb-err") {
			t.Fatalf("failed balancer must not emit selected metric: %v", sel)
		}
	}
	states := sampleValues(t, m, "xui_balancer_state")
	if len(states) != 1 || states["balancer=lb-ok;"] != 2 {
		t.Fatalf("expected lb-ok state metric to be preserved, got: %v", states)
	}
	for k := range states {
		if strings.Contains(k, "lb-err") {
			t.Fatalf("failed balancer must not emit state metric: %v", states)
		}
	}
}

func TestBalancerState(t *testing.T) {
	tests := []struct {
		name          string
		cfgJSON       string
		tags          map[string]bool
		state         *BalancerState
		expectedState float64
		expectMetric  bool
	}{
		{
			name:          "primary active",
			cfgJSON:       `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"fb","strategy":{"type":"leastLoad"}}]}}`,
			tags:          map[string]bool{"A": true, "fb": true},
			state:         &BalancerState{Targets: map[string]bool{"A": true}},
			expectedState: 2,
			expectMetric:  true,
		},
		{
			name:          "fallback active",
			cfgJSON:       `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"fb","strategy":{"type":"leastLoad"}}]}}`,
			tags:          map[string]bool{"A": true, "fb": true},
			state:         &BalancerState{Targets: map[string]bool{}},
			expectedState: 1,
			expectMetric:  true,
		},
		{
			name:          "override active",
			cfgJSON:       `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"fb","strategy":{"type":"leastLoad"}}]}}`,
			tags:          map[string]bool{"A": true, "fb": true, "direct": true},
			state:         &BalancerState{Targets: map[string]bool{"direct": true}, Override: true},
			expectedState: 3,
			expectMetric:  true,
		},
		{
			name:          "unavailable no fallback",
			cfgJSON:       `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"strategy":{"type":"leastLoad"}}]}}`,
			tags:          map[string]bool{"A": true},
			state:         &BalancerState{Targets: map[string]bool{}},
			expectedState: 0,
			expectMetric:  true,
		},
		{
			name:          "unavailable fallback dead or missing",
			cfgJSON:       `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"nonexistent","strategy":{"type":"leastLoad"}}]}}`,
			tags:          map[string]bool{"A": true},
			state:         &BalancerState{Targets: map[string]bool{}},
			expectedState: 0,
			expectMetric:  true,
		},
		{
			name:          "nil state emits no metric",
			cfgJSON:       `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"strategy":{"type":"leastLoad"}}]}}`,
			tags:          map[string]bool{"A": true},
			state:         nil,
			expectedState: 0,
			expectMetric:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configuration(t, tc.cfgJSON)
			e := &Exporter{Core: &fakeCore{tags: tc.tags, states: map[string]*BalancerState{"lb": tc.state}}}
			m, err := e.balancerMetrics(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			vals := sampleValues(t, m, "xui_balancer_state")
			if !tc.expectMetric {
				if len(vals) != 0 {
					t.Fatalf("expected no xui_balancer_state metric, got: %v", vals)
				}
				return
			}
			val, ok := vals["balancer=lb;"]
			if !ok {
				t.Fatalf("missing xui_balancer_state for balancer=lb, got: %v", vals)
			}
			if val != tc.expectedState {
				t.Fatalf("expected state %v, got %v", tc.expectedState, val)
			}
		})
	}
}

func TestBalancerOperationalHealth(t *testing.T) {
	tests := []struct {
		name           string
		cfgJSON        string
		tags           map[string]bool
		states         map[string]*BalancerState
		observations   map[string]bool
		expectedHealth map[string]float64
		expectedState  map[string]float64
	}{
		{
			name:    "primary alive in observatory",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"direct","strategy":{"type":"leastLoad"}}]}}`,
			tags:    map[string]bool{"A": true, "direct": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{"A": true}}},
			observations: map[string]bool{
				"A": true,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 2},
			expectedState:  map[string]float64{"balancer=lb;": 2},
		},
		{
			name:    "primary unmonitored is blind",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"direct","strategy":{"type":"leastLoad"}}]}}`,
			tags:    map[string]bool{"A": true, "direct": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{"A": true}}},
			observations: map[string]bool{
				"other": true,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 1},
			expectedState:  map[string]float64{"balancer=lb;": 2},
		},
		{
			name:    "primary dead in observatory drops to down",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"direct","strategy":{"type":"leastLoad"}}]}}`,
			tags:    map[string]bool{"A": true, "direct": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{"A": true}}},
			observations: map[string]bool{
				"A": false,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 0},
			expectedState:  map[string]float64{"balancer=lb;": 2},
		},
		{
			name:    "fallback to unprobed direct is blind",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"direct","strategy":{"type":"leastLoad"}}]}}`,
			tags:    map[string]bool{"A": true, "direct": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{}}},
			observations: map[string]bool{
				"A": false,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 1},
			expectedState:  map[string]float64{"balancer=lb;": 1},
		},
		{
			name:    "fallback to probed alive proxy is up",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"backup","strategy":{"type":"leastLoad"}}]}}`,
			tags:    map[string]bool{"A": true, "backup": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{}}},
			observations: map[string]bool{
				"A":      false,
				"backup": true,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 2},
			expectedState:  map[string]float64{"balancer=lb;": 1},
		},
		{
			name:    "fallback to probed dead outbound drops to down",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"backup","strategy":{"type":"leastLoad"}}]}}`,
			tags:    map[string]bool{"A": true, "backup": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{}}},
			observations: map[string]bool{
				"A":      false,
				"backup": false,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 0},
			expectedState:  map[string]float64{"balancer=lb;": 1},
		},
		{
			name:    "fallback to blackhole is down",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"blocked","strategy":{"type":"leastPing"}}]},"outbounds":[{"tag":"blocked","protocol":"blackhole"}]}`,
			tags:    map[string]bool{"A": true, "blocked": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{}}},
			observations: map[string]bool{
				"A": false,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 0},
			expectedState:  map[string]float64{"balancer=lb;": 1},
		},
		{
			name:    "cascaded balancers via loopback - sub alive",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb-main","selector":["A"],"fallbackTag":"to-sub","strategy":{"type":"leastLoad"}},{"tag":"lb-sub","selector":["B"],"fallbackTag":"direct","strategy":{"type":"leastLoad"}}],"rules":[{"inboundTag":["sub-in"],"balancerTag":"lb-sub"}]},"outbounds":[{"tag":"to-sub","protocol":"loopback","settings":{"inboundTag":"sub-in"}}]}`,
			tags:    map[string]bool{"A": true, "to-sub": true, "B": true, "direct": true},
			states: map[string]*BalancerState{
				"lb-main": {Targets: map[string]bool{}},
				"lb-sub":  {Targets: map[string]bool{"B": true}},
			},
			observations: map[string]bool{
				"A": false,
				"B": true,
			},
			expectedHealth: map[string]float64{"balancer=lb-main;": 2, "balancer=lb-sub;": 2},
			expectedState:  map[string]float64{"balancer=lb-main;": 1, "balancer=lb-sub;": 2},
		},
		{
			name:    "cascaded balancers via loopback - sub dead drops main to down",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb-main","selector":["A"],"fallbackTag":"to-sub","strategy":{"type":"leastLoad"}},{"tag":"lb-sub","selector":["B"],"fallbackTag":"backup-dead","strategy":{"type":"leastLoad"}}],"rules":[{"inboundTag":["sub-in"],"balancerTag":"lb-sub"}]},"outbounds":[{"tag":"to-sub","protocol":"loopback","settings":{"inboundTag":"sub-in"}}]}`,
			tags:    map[string]bool{"A": true, "to-sub": true, "B": true, "backup-dead": true},
			states: map[string]*BalancerState{
				"lb-main": {Targets: map[string]bool{}},
				"lb-sub":  {Targets: map[string]bool{"B": true}},
			},
			observations: map[string]bool{
				"A":           false,
				"B":           false,
				"backup-dead": false,
			},
			expectedHealth: map[string]float64{"balancer=lb-main;": 0, "balancer=lb-sub;": 0},
			expectedState:  map[string]float64{"balancer=lb-main;": 1, "balancer=lb-sub;": 2},
		},
		{
			name:    "multi-target candidate partial health (random/roundrobin)",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A","B"],"fallbackTag":"direct","strategy":{"type":"random"}}]}}`,
			tags:    map[string]bool{"A": true, "B": true, "direct": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{"A": true, "B": true}}},
			observations: map[string]bool{
				"A": false,
				"B": true,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 2},
			expectedState:  map[string]float64{"balancer=lb;": 2},
		},
		{
			name:    "override target alive is up",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"direct","strategy":{"type":"leastLoad"}}]}}`,
			tags:    map[string]bool{"A": true, "override-out": true, "direct": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{"override-out": true}, Override: true}},
			observations: map[string]bool{
				"override-out": true,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 2},
			expectedState:  map[string]float64{"balancer=lb;": 3},
		},
		{
			name:    "override target unmonitored is blind",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"direct","strategy":{"type":"leastLoad"}}]}}`,
			tags:    map[string]bool{"A": true, "override-out": true, "direct": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{"override-out": true}, Override: true}},
			observations: map[string]bool{
				"other": true,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 1},
			expectedState:  map[string]float64{"balancer=lb;": 3},
		},
		{
			name:    "override target dead drops to down",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"direct","strategy":{"type":"leastLoad"}}]}}`,
			tags:    map[string]bool{"A": true, "override-out": true, "direct": true},
			states:  map[string]*BalancerState{"lb": {Targets: map[string]bool{"override-out": true}, Override: true}},
			observations: map[string]bool{
				"override-out": false,
			},
			expectedHealth: map[string]float64{"balancer=lb;": 0},
			expectedState:  map[string]float64{"balancer=lb;": 3},
		},
		{
			name:    "cascaded cycle detection drops to down",
			cfgJSON: `{"routing":{"balancers":[{"tag":"lb-a","selector":["A"],"fallbackTag":"to-b","strategy":{"type":"leastLoad"}},{"tag":"lb-b","selector":["B"],"fallbackTag":"to-a","strategy":{"type":"leastLoad"}}],"rules":[{"inboundTag":["b-in"],"balancerTag":"lb-b"},{"inboundTag":["a-in"],"balancerTag":"lb-a"}]},"outbounds":[{"tag":"to-b","protocol":"loopback","settings":{"inboundTag":"b-in"}},{"tag":"to-a","protocol":"loopback","settings":{"inboundTag":"a-in"}}]}`,
			tags:    map[string]bool{"A": true, "B": true, "to-b": true, "to-a": true},
			states: map[string]*BalancerState{
				"lb-a": {Targets: map[string]bool{}},
				"lb-b": {Targets: map[string]bool{}},
			},
			observations:   map[string]bool{},
			expectedHealth: map[string]float64{"balancer=lb-a;": 0, "balancer=lb-b;": 0},
			expectedState:  map[string]float64{"balancer=lb-a;": 0, "balancer=lb-b;": 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configuration(t, tc.cfgJSON)
			m, err := buildBalancerMetrics(cfg, tc.tags, tc.states, tc.observations)
			if err != nil {
				t.Fatal(err)
			}
			healthVals := sampleValues(t, m, "xui_balancer_health")
			for k, want := range tc.expectedHealth {
				got, ok := healthVals[k]
				if !ok {
					t.Fatalf("missing xui_balancer_health for %s, got: %v", k, healthVals)
				}
				if got != want {
					t.Fatalf("for %s health expected %v, got %v", k, want, got)
				}
			}
			stateVals := sampleValues(t, m, "xui_balancer_state")
			for k, want := range tc.expectedState {
				got, ok := stateVals[k]
				if !ok {
					t.Fatalf("missing xui_balancer_state for %s, got: %v", k, stateVals)
				}
				if got != want {
					t.Fatalf("for %s state expected %v, got %v", k, want, got)
				}
			}
		})
	}
}

func TestExporterProcessAndGoMetrics(t *testing.T) {
	e := &Exporter{Panel: fakePanel{cfg: &Config{}}, Timeout: time.Second}
	h := e.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Fatal("missing go runtime metrics")
	}
	if !strings.Contains(rec.Body.String(), "process_resident_memory_bytes") {
		t.Fatal("missing process metrics")
	}
}

type fakePanelWithOnlines struct {
	fakePanel
	onlines    []string
	onlinesErr error
}

func (f fakePanelWithOnlines) Onlines(context.Context) ([]string, error) {
	return f.onlines, f.onlinesErr
}

func TestClientOnlineMetric(t *testing.T) {
	eWith := &Exporter{Panel: fakePanelWithOnlines{fakePanel: fakePanel{cfg: &Config{}}, onlines: []string{"a", "b", "c", "d", "e"}}, Timeout: time.Second}
	mWith := eWith.scrape(context.Background())
	valsWith := sampleValues(t, mWith, "xui_client_online")
	if len(valsWith) != 1 || valsWith[""] != 5 {
		t.Fatalf("expected 5 online clients, got %v", valsWith)
	}
	if sampleValues(t, mWith, "xui_exporter_scrape_success")["source=api;"] != 1 {
		t.Fatal("expected successful API scrape")
	}

	eErr := &Exporter{Panel: fakePanelWithOnlines{fakePanel: fakePanel{cfg: &Config{}}, onlinesErr: errors.New("fail")}, Timeout: time.Second}
	mErr := eErr.scrape(context.Background())
	valsErr := sampleValues(t, mErr, "xui_client_online")
	if len(valsErr) != 0 {
		t.Fatalf("expected no online clients on error, got %v", valsErr)
	}
	if sampleValues(t, mErr, "xui_exporter_scrape_success")["source=api;"] != 0 {
		t.Fatal("expected failed API scrape on onlines error")
	}
}

type fakePanelWithInbounds struct {
	fakePanel
	inbounds    []*InboundStatus
	inboundsErr error
}

func (f fakePanelWithInbounds) Inbounds(context.Context) ([]*InboundStatus, error) {
	return f.inbounds, f.inboundsErr
}

func TestInboundEnabledMetric(t *testing.T) {
	inbs := []*InboundStatus{
		{
			Tag:    "vless-in",
			Enable: true,
			Clients: []*ClientStatus{
				{Email: "active@x.com", Enable: true},
				{Email: "disabled@x.com", Enable: false},
			},
		},
		{
			Tag:    "vmess-in",
			Enable: false,
			Clients: []*ClientStatus{
				{Email: "expired@x.com", Enable: true, ExpiryTime: 1000}, // expired in past
				{Email: "depleted@x.com", Enable: true, Total: 100, Up: 50, Down: 50},
			},
		},
	}
	eWith := &Exporter{Panel: fakePanelWithInbounds{fakePanel: fakePanel{cfg: &Config{}}, inbounds: inbs}, Timeout: time.Second}
	mWith := eWith.scrape(context.Background())
	valsWith := sampleValues(t, mWith, "xui_inbound_enabled")
	if len(valsWith) != 2 || valsWith["inbound=vless-in;node=local;remark=vless-in;"] != 1 || valsWith["inbound=vmess-in;node=local;remark=vmess-in;"] != 0 {
		t.Fatalf("unexpected inbounds metric: %v", valsWith)
	}
	clientCounts := sampleValues(t, mWith, "xui_client_count")
	if len(clientCounts) != 4 || clientCounts["state=active;"] != 1 || clientCounts["state=disabled;"] != 1 || clientCounts["state=expired;"] != 1 || clientCounts["state=depleted;"] != 1 {
		t.Fatalf("unexpected client count metrics: %v", clientCounts)
	}
	if sampleValues(t, mWith, "xui_exporter_scrape_success")["source=api;"] != 1 {
		t.Fatal("expected successful API scrape")
	}

	eErr := &Exporter{Panel: fakePanelWithInbounds{fakePanel: fakePanel{cfg: &Config{}}, inboundsErr: errors.New("fail")}, Timeout: time.Second}
	mErr := eErr.scrape(context.Background())
	valsErr := sampleValues(t, mErr, "xui_inbound_enabled")
	if len(valsErr) != 0 {
		t.Fatalf("expected no inbounds metric on error, got %v", valsErr)
	}
	if len(sampleValues(t, mErr, "xui_client_count")) != 0 {
		t.Fatalf("expected no client count metric on inbounds error")
	}
	if sampleValues(t, mErr, "xui_exporter_scrape_success")["source=api;"] != 0 {
		t.Fatal("expected failed API scrape on inbounds error")
	}
}

type fakePanelWithNodes struct {
	fakePanel
	nodes    []*NodeStatus
	nodesErr error
}

func (f fakePanelWithNodes) Nodes(context.Context) ([]*NodeStatus, error) {
	return f.nodes, f.nodesErr
}

type fakePanelFull struct {
	fakePanel
	inbounds []*InboundStatus
	nodes    []*NodeStatus
	onlines  []string
}

func (f fakePanelFull) Inbounds(context.Context) ([]*InboundStatus, error) {
	return f.inbounds, nil
}

func (f fakePanelFull) Nodes(context.Context) ([]*NodeStatus, error) {
	return f.nodes, nil
}

func (f fakePanelFull) Onlines(context.Context) ([]string, error) {
	return f.onlines, nil
}

func TestNodeMetricsAndInboundEnrichment(t *testing.T) {
	nodeID := 3
	up := int64(1048576)
	down := int64(20971520)
	onlineCount := 8
	nodes := []*NodeStatus{
		{ID: 3, Name: "node-remote", Status: "online", LatencyMs: 50, XrayState: "running", OnlineCount: &onlineCount},
	}
	inbs := []*InboundStatus{
		{Tag: "n3-in", Remark: "vless-remote", NodeID: &nodeID, Enable: true, Up: &up, Down: &down},
		{Tag: "in-local", Remark: "tun-local", Enable: true, ClientEmails: []string{"local-user@test.com"}},
	}
	eFull := &Exporter{
		Panel: fakePanelFull{
			fakePanel: fakePanel{cfg: &Config{}},
			inbounds:  inbs,
			nodes:     nodes,
			onlines:   []string{"local-user@test.com", "other@test.com"},
		},
		Timeout: time.Second,
	}
	m := eFull.scrape(context.Background())
	inbVals := sampleValues(t, m, "xui_inbound_enabled")
	if inbVals["inbound=n3-in;node=node-remote;remark=vless-remote;"] != 1 {
		t.Fatalf("unexpected enriched inbound metric: %v", inbVals)
	}
	if inbVals["inbound=in-local;node=local;remark=tun-local;"] != 1 {
		t.Fatalf("unexpected local inbound metric: %v", inbVals)
	}
	inbTraffic := sampleValues(t, m, "xui_inbound_bytes_total")
	if inbTraffic["direction=uplink;inbound=n3-in;node=node-remote;remark=vless-remote;"] != 1048576 {
		t.Fatalf("unexpected inbound uplink traffic: %v", inbTraffic)
	}
	if inbTraffic["direction=downlink;inbound=n3-in;node=node-remote;remark=vless-remote;"] != 20971520 {
		t.Fatalf("unexpected inbound downlink traffic: %v", inbTraffic)
	}
	nodeVals := sampleValues(t, m, "xui_node_online")
	if nodeVals["node=node-remote;"] != 1 {
		t.Fatalf("unexpected node online metric: %v", nodeVals)
	}
	nodeOnlineClients := sampleValues(t, m, "xui_node_online_clients")
	if nodeOnlineClients["node=node-remote;"] != 8 {
		t.Fatalf("unexpected node online clients: %v", nodeOnlineClients)
	}
	if nodeOnlineClients["node=local;"] != 1 {
		t.Fatalf("unexpected local online clients: %v", nodeOnlineClients)
	}

	eErr := &Exporter{Panel: fakePanelWithNodes{fakePanel: fakePanel{cfg: &Config{}}, nodesErr: errors.New("nodes error")}, Timeout: time.Second}
	mErr := eErr.scrape(context.Background())
	if sampleValues(t, mErr, "xui_exporter_scrape_success")["source=api;"] != 0 {
		t.Fatal("expected failed API scrape on nodes error")
	}
	if len(sampleValues(t, mErr, "xui_node_online")) != 0 {
		t.Fatal("expected no node metrics on error")
	}
}

func TestExporterSlowThreshold(t *testing.T) {
	tests := []struct {
		timeout time.Duration
		want    time.Duration
	}{
		{4 * time.Second, 1 * time.Second},
		{10 * time.Second, 2500 * time.Millisecond},
		{2 * time.Second, 500 * time.Millisecond},
		{0, 1 * time.Second},
		{-1 * time.Second, 1 * time.Second},
	}
	for _, tt := range tests {
		e := &Exporter{Timeout: tt.timeout}
		if got := e.slowThreshold(); got != tt.want {
			t.Errorf("slowThreshold() for timeout %v = %v, want %v", tt.timeout, got, tt.want)
		}
	}

	var nilExporter *Exporter
	if got := nilExporter.slowThreshold(); got != 1*time.Second {
		t.Errorf("nil Exporter slowThreshold() = %v, want 1s", got)
	}

	var nilCore *GRPCCore
	if got := nilCore.threshold(); got != 1*time.Second {
		t.Errorf("nil GRPCCore threshold() = %v, want 1s", got)
	}

	c := &GRPCCore{slowThreshold: 500 * time.Millisecond}
	if got := c.threshold(); got != 500*time.Millisecond {
		t.Errorf("GRPCCore threshold() = %v, want 500ms", got)
	}
}

func TestConcurrentScrapesWithGRPCCoreNoDataRace(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	svc := rpcServer{}
	hc.RegisterHandlerServiceServer(server, svc)
	rc.RegisterRoutingServiceServer(server, svc)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	conn, err := grpc.NewClient(
		"passthrough:///test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	cfg := configuration(t, `{"metrics":{"listen":"localhost:11111"},"routing":{"balancers":[{"tag":"lb1","selector":["A"],"strategy":{"type":"leastLoad"}},{"tag":"lb2","selector":["A"],"strategy":{"type":"leastPing"}}]}}`)
	e := &Exporter{
		Panel:   fakePanel{cfg: cfg},
		Core:    newCore(conn),
		Expvars: staticExpvars{},
		Timeout: 2 * time.Second,
		Version: "test",
	}
	handler := e.Handler()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.Contains(body, `xui_exporter_scrape_success{source="grpc"} 1`) {
				t.Errorf("unexpected response (%d): %s", w.Code, body)
			}
		}()
	}
	wg.Wait()
}
