package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type staticExpvars struct{}

func (staticExpvars) Snapshot(context.Context, *Config) (*ExpvarsSnapshot, error) {
	return &ExpvarsSnapshot{TrafficPresent: true, Traffic: []*TrafficCounter{{Name: "outbound>>>A>>>traffic>>>uplink", Value: 0}}}, nil
}

type countedCore struct {
	fakeCore
	lists, balancers int
}

func (c *countedCore) Outbounds(ctx context.Context) (map[string]bool, error) {
	c.lists++
	return c.fakeCore.Outbounds(ctx)
}
func (c *countedCore) Balancer(ctx context.Context, tag string) (*BalancerState, error) {
	c.balancers++
	return c.fakeCore.Balancer(ctx, tag)
}
func TestOneSnapshotWithoutSourceFallback(t *testing.T) {
	for _, tc := range []struct {
		name, body             string
		status                 int
		disabled               bool
		trafficCount, sourceOK int
	}{
		{"healthy", `{"stats":{"outbound":{"A":{"uplink":0}}},"memstats":{"Alloc":123},"observatory":null}`, 200, false, 1, 1},
		{"empty counters", `{"stats":{}}`, 200, false, 0, 1},
		{"no observer", `{"stats":{},"memstats":{"NumGC":0}}`, 200, false, 0, 1},
		{"missing counters", `{"memstats":{"Alloc":123}}`, 200, false, 0, 1},
		{"invalid counters", `{"stats":{"outbound":{"A":{"uplink":-1}}},"memstats":{"Alloc":123}}`, 200, false, 0, 0},
		{"unreachable", ``, 503, false, 0, 0},
		{"disabled but explicit endpoint", `{}`, 200, true, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			cfg := configuration(t, `{"metrics":{"listen":"localhost:11111"},"routing":{"balancers":[{"tag":"one","selector":["A"],"strategy":{"type":"leastLoad"}},{"tag":"two","selector":["A"],"strategy":{"type":"leastLoad"}}]}}`)
			if tc.disabled {
				cfg.Metrics.Listen = ""
			}
			c := &countedCore{fakeCore: fakeCore{tags: map[string]bool{"A": true}, states: map[string]*BalancerState{"one": {Targets: map[string]bool{"A": true}}, "two": {Targets: map[string]bool{"A": true}}}}}
			e := &Exporter{Panel: fakePanel{cfg: cfg}, Core: c, Expvars: &Expvars{URL: srv.URL, Client: srv.Client()}}
			m := e.scrape(context.Background())
			if c.lists != 1 || c.balancers != 2 {
				t.Fatalf("RPC counts list=%d balancers=%d", c.lists, c.balancers)
			}
			if got := len(sampleValues(t, m, "xui_outbound_traffic_bytes_total")); got != tc.trafficCount {
				t.Fatalf("traffic count=%d, want %d", got, tc.trafficCount)
			}
			wantHTTP := int64(1)
			if calls.Load() != wantHTTP {
				t.Fatalf("HTTP calls=%d", calls.Load())
			}
			status := sampleValues(t, m, "xui_exporter_scrape_success")
			if len(status) != 3 || status["source=expvars;"] != float64(tc.sourceOK) {
				t.Fatal(status)
			}

			for _, name := range []string{"xui_xray_uptime_seconds", "xui_xray_goroutines"} {
				if len(sampleValues(t, m, name)) != 0 {
					t.Fatal("removed metric present", name)
				}
			}
			if strings.Contains(tc.body, `"Alloc":123`) && len(sampleValues(t, m, "xui_xray_memory_alloc_bytes")) != 1 {
				t.Fatal("valid memory lost")
			}
		})
	}
}

func TestMemoryPresenceAndPartialGroups(t *testing.T) {
	data, err := decodeExpvars([]byte(`{"stats":{"outbound":{"original-tag":{"uplink":0,"downlink":null}},"user":{"private":{"uplink":999}}},"memstats":{"Alloc":0,"HeapObjects":23,"PauseTotalNs":250000000},"observatory":{"A":null}}`))
	if err != nil || data.Err == nil || !data.TrafficPresent {
		t.Fatal(data, err)
	}
	if len(data.Memory) != 3 || len(data.Traffic) != 1 || len(data.Observation) != 0 {
		t.Fatalf("unexpected groups: %+v", data)
	}
	if v := sampleValues(t, data.Memory, "xui_xray_gc_pause_seconds_total")[""]; v != .25 {
		t.Fatal(v)
	}
	if v := sampleValues(t, data.Memory, "xui_xray_memory_live_objects")[""]; v != 23 {
		t.Fatal(v)
	}
	if len(sampleValues(t, data.Memory, "xui_xray_memory_sys_bytes")) != 0 {
		t.Fatal("missing memory synthesized")
	}
	m, err := trafficMetrics(data.Traffic)
	if err != nil {
		t.Fatal(err)
	}
	if v := sampleValues(t, m, "xui_outbound_traffic_bytes_total"); len(v) != 1 || v["direction=uplink;outbound=original-tag;"] != 0 {
		t.Fatal(v)
	}
	for _, raw := range []string{`{"Alloc":-1}`, `{"Alloc":1.5}`, `[]`} {
		if _, err := expvarsMemory([]byte(raw)); err == nil {
			t.Fatal("invalid memory accepted", raw)
		}
	}
}
