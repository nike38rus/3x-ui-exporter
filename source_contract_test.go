package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestMetricsEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, explicit, listen, want string
		noConfig, bad                bool
	}{
		{name: "explicit without API", explicit: "https://metrics.example/base", want: "https://metrics.example/base", noConfig: true},
		{name: "explicit overrides disabled", explicit: "http://localhost:9000", want: "http://localhost:9000"},
		{name: "IPv4", listen: "127.0.0.1:11111", want: "http://127.0.0.1:11111"},
		{name: "wildcard IPv4", listen: "0.0.0.0:11111", want: "http://127.0.0.1:11111"},
		{name: "wildcard IPv6", listen: "[::]:11111", want: "http://[::1]:11111"},
		{name: "IPv6", listen: "[2001:db8::1]:1234", want: "http://[2001:db8::1]:1234"},
		{name: "hostname", listen: "xray:11111", want: "http://xray:11111"},
		{name: "empty bind", listen: ":11111", want: "http://127.0.0.1:11111"},
		{name: "no config", noConfig: true, bad: true},
		{name: "disabled", bad: true},
		{name: "bad port", listen: "127.0.0.1:0", bad: true},
		{name: "oversized port", listen: "127.0.0.1:65536", bad: true},
		{name: "service port", listen: "127.0.0.1:http", bad: true},
		{name: "not address", listen: "/tmp/metrics", bad: true},
		{name: "path in host", listen: "host/path:11111", bad: true},
		{name: "credentials", explicit: "http://user:secret@host:11111", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Metrics.Listen = tc.listen
			if tc.noConfig {
				cfg = nil
			}
			got, err := metricsEndpoint(tc.explicit, cfg)
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %q, %v; want %q, bad=%v", got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestFundamentalSourceStatus(t *testing.T) {
	for _, tc := range []struct {
		name              string
		cfg               *Config
		apiErr            error
		grpcFail          bool
		wantAPI, wantGRPC float64
	}{
		{name: "no balancers", cfg: &Config{}, wantAPI: 1, wantGRPC: 1},
		{name: "API failure", apiErr: errors.New("unavailable"), wantGRPC: 1},
		{name: "nil configuration", wantGRPC: 1},
		{name: "gRPC failure", cfg: &Config{}, grpcFail: true, wantAPI: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := &countedCore{fakeCore: fakeCore{fail: tc.grpcFail}}
			e := &Exporter{Panel: fakePanel{cfg: tc.cfg, err: tc.apiErr}, Core: core, Expvars: staticExpvars{}}
			m := e.scrape(context.Background())
			ok := sampleValues(t, m, "xui_exporter_scrape_success")
			if len(ok) != 3 || ok["source=api;"] != tc.wantAPI || ok["source=grpc;"] != tc.wantGRPC || ok["source=expvars;"] != 1 {
				t.Fatal(ok)
			}
			if len(sampleValues(t, m, "xui_exporter_scrape_duration_seconds")) != 3 {
				t.Fatal("missing duration")
			}
			if core.lists != 1 || core.balancers != 0 {
				t.Fatal("incorrect RPC budget", core)
			}
		})
	}
}

func TestDiscoveryUsesOnlyCurrentConfiguration(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if r.URL.Path != "/debug/vars" || r.Header.Get("Authorization") != "" {
			t.Error("wrong path or leaked token")
		}
		_, _ = w.Write([]byte(`{"memstats":{"Alloc":123}}`))
	}))
	defer server.Close()
	cfg := &Config{}
	cfg.Metrics.Listen = strings.TrimPrefix(server.URL, "http://")
	e := &Exporter{Panel: fakePanel{cfg: cfg}, Core: &fakeCore{}, Expvars: &Expvars{Client: server.Client()}}
	first := e.scrape(context.Background())
	if sampleValues(t, first, "xui_exporter_scrape_success")["source=expvars;"] != 1 {
		t.Fatal("discovery failed")
	}
	cfg.Metrics.Listen = ""
	for _, panel := range []fakePanel{{cfg: cfg}, {err: errors.New("API failed")}} {
		e.Panel = panel
		m := e.scrape(context.Background())
		if sampleValues(t, m, "xui_exporter_scrape_success")["source=expvars;"] != 0 {
			t.Fatal("missing endpoint accepted")
		}
		if len(sampleValues(t, m, "xui_xray_memory_alloc_bytes")) != 0 {
			t.Fatal("stale memory retained")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatal("reused old endpoint", calls)
	}
}

func TestDiscoveryConcurrentAddresses(t *testing.T) {
	servers := []*httptest.Server{
		httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"memstats":{"Alloc":1}}`)) })),
		httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"memstats":{"Alloc":2}}`)) })),
	}
	for _, s := range servers {
		defer s.Close()
	}
	source := &Expvars{Client: servers[0].Client()}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg := &Config{}
			cfg.Metrics.Listen = strings.TrimPrefix(servers[i%2].URL, "http://")
			data, err := source.Snapshot(context.Background(), cfg)
			if err != nil {
				t.Error(err)
				return
			}
			if got := sampleValues(t, data.Memory, "xui_xray_memory_alloc_bytes")[""]; got != float64(i%2+1) {
				t.Errorf("cross-scrape endpoint leakage: %v", got)
			}
		}(i)
	}
	wg.Wait()
}

type failedBalancerCore struct{ fakeCore }

func (c *failedBalancerCore) Balancer(context.Context, string) (*BalancerState, error) {
	return nil, errors.New("RPC failed")
}

func TestGRPCRequestAndDerivationFailures(t *testing.T) {
	cfg := configuration(t, `{"routing":{"balancers":[{"tag":"lb","selector":["A"],"fallbackTag":"loop","strategy":{"type":"leastLoad"}}],"rules":[{"inboundTag":["loop-in"],"outboundTag":"A","port":"443"}]},"outbounds":[{"tag":"loop","protocol":"loopback","settings":{"inboundTag":"loop-in"}}]}`)
	for _, tc := range []struct {
		name string
		core Core
		want float64
	}{
		{"RPC error", &failedBalancerCore{}, 0},
		{"configuration derivation error", &fakeCore{tags: map[string]bool{"A": true, "loop": true}, states: map[string]*BalancerState{"lb": {}}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Exporter{Panel: fakePanel{cfg: cfg}, Core: tc.core, Expvars: staticExpvars{}}
			m := e.scrape(context.Background())
			if got := sampleValues(t, m, "xui_exporter_scrape_success")["source=grpc;"]; got != tc.want {
				t.Fatalf("grpc=%v want %v", got, tc.want)
			}
			if len(sampleValues(t, m, "xui_balancer_outbound_selected")) != 0 {
				t.Fatal("invalid derived data published")
			}
		})
	}
}
