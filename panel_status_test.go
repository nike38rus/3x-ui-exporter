package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPanelStatusFields(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		wantErr            bool
		panel, core, state string
		tcp, udp           *float64
	}{
		{"running", `{"panelVersion":"3.10.2","xray":{"version":"26.10.10","state":"running","errorMsg":"private diagnostic"},"tcpCount":42,"udpCount":18}`, false, "3.10.2", "26.10.10", "running", ptr(42), ptr(18)},
		{"stopped", `{"xray":{"state":"stop"}}`, false, "", "", "stop", nil, nil},
		{"error", `{"xray":{"state":"error"}}`, false, "", "", "error", nil, nil},
		{"missing", `{}`, false, "", "", "", nil, nil},
		{"null fields", `{"panelVersion":null,"xray":null,"tcpCount":null,"udpCount":null}`, false, "", "", "", nil, nil},
		{"unknown state", `{"panelVersion":"p","xray":{"version":"x","state":"future"}}`, true, "p", "x", "", nil, nil},
		{"bad version keeps state", `{"panelVersion":12,"xray":{"state":"running"}}`, true, "", "", "running", nil, nil},
		{"bad Xray keeps panel", `{"panelVersion":"p","xray":[]}`, true, "p", "", "", nil, nil},
		{"invalid object", `[]`, true, "", "", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := decodePanelStatus(json.RawMessage(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatal(err)
			}
			m := panelStatusMetrics(s)
			for name, version := range map[string]string{"xui_panel_info": tc.panel, "xui_xray_info": tc.core} {
				values := sampleValues(t, m, name)
				if version == "" {
					if len(values) != 0 {
						t.Fatal(values)
					}
				} else if len(values) != 1 || values["version="+version+";"] != 1 {
					t.Fatal(values)
				}
			}
			states := sampleValues(t, m, "xui_xray_state")
			if tc.state == "" {
				if len(states) != 0 {
					t.Fatal(states)
				}
			} else {
				if len(states) != 3 {
					t.Fatal(states)
				}
				for _, state := range []string{"running", "stop", "error"} {
					if v, ok := states["state="+state+";"]; !ok || v != number(state == tc.state) {
						t.Fatal(states)
					}
				}
			}
			sockets := sampleValues(t, m, "xui_open_sockets")
			if tc.tcp == nil {
				if _, ok := sockets["protocol=tcp;"]; ok {
					t.Fatal("unexpected tcp sockets", sockets)
				}
			} else if sockets["protocol=tcp;"] != *tc.tcp {
				t.Fatal("incorrect tcp count", sockets)
			}
			if tc.udp == nil {
				if _, ok := sockets["protocol=udp;"]; ok {
					t.Fatal("unexpected udp sockets", sockets)
				}
			} else if sockets["protocol=udp;"] != *tc.udp {
				t.Fatal("incorrect udp count", sockets)
			}
		})
	}
}

func ptr(v float64) *float64 { return &v }

func TestPanelOnlines(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
		want int
		fail bool
	}{
		{"active users", `{"success":true,"obj":["user1@test.com","user2@test.com"]}`, 200, 2, false},
		{"empty list", `{"success":true,"obj":[]}`, 200, 0, false},
		{"invalid json", `{"success":true,"obj":"not an array"}`, 200, 0, true},
		{"server error", `server down`, 500, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/panel/api/clients/onlines" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := &Panel{URL: srv.URL, Token: "token", Client: srv.Client()}
			got, err := p.Onlines(context.Background())
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v, fail=%v", err, tc.fail)
			}
			if got != tc.want {
				t.Fatalf("got=%d, want=%d", got, tc.want)
			}
		})
	}
}

func TestPanelStatusRequestAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		fail       bool
	}{
		{"valid", `{"success":true,"obj":{"xray":{"state":"running"}}}`, 200, false},
		{"unauthorized", `private body`, 401, true},
		{"unsuccessful", `{"success":false,"obj":{}}`, 200, true},
		{"missing", `{"success":true}`, 200, true},
		{"invalid JSON", `invalid`, 200, true},
		{"oversized", strings.Repeat("x", (16<<20)+1), 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/base/panel/api/server/status" || r.Header.Get("Authorization") != "Bearer token" {
					t.Error("incorrect status request")
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := Panel{URL: srv.URL + "/base/", Token: "token", Client: srv.Client()}
			_, err := p.Status(context.Background())
			if (err != nil) != tc.fail {
				t.Fatal(err)
			}
		})
	}
}

func TestPanelRequestsIndependentAndBudget(t *testing.T) {
	for _, failed := range []string{"", "settings", "status", "inbounds", "nodes", "onlines"} {
		t.Run(failed, func(t *testing.T) {
			counts := map[string]int{}
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				counts[r.URL.Path]++
				mu.Unlock()
				switch r.URL.Path {
				case "/panel/api/xray/":
					if failed == "settings" {
						w.WriteHeader(503)
						return
					}
					_, _ = w.Write([]byte(`{"success":true,"obj":{"xraySetting":{"metrics":{"listen":"enabled"},"routing":{"balancers":[{"tag":"lb","selector":["A"],"strategy":{"type":"leastLoad"}}]}}}}`))
				case "/panel/api/server/status":
					if failed == "status" {
						w.WriteHeader(503)
						return
					}
					_, _ = w.Write([]byte(`{"success":true,"obj":{"panelVersion":"p","xray":{"version":"x","state":"running"}}}`))
				case "/panel/api/clients/onlines":
					if failed == "onlines" {
						w.WriteHeader(503)
						return
					}
					_, _ = w.Write([]byte(`{"success":true,"obj":["u@test.com"]}`))
				case "/panel/api/inbounds/list":
					if failed == "inbounds" {
						w.WriteHeader(503)
						return
					}
					_, _ = w.Write([]byte(`{"success":true,"obj":[{"tag":"inbound-443","enable":true},{"tag":"inbound-80","enable":false}]}`))
				case "/panel/api/nodes/list":
					if failed == "nodes" {
						w.WriteHeader(503)
						return
					}
					_, _ = w.Write([]byte(`{"success":true,"obj":[{"id":1,"name":"node-1","status":"online"}]}`))
				case "/debug/vars":
					_, _ = w.Write([]byte(`{"stats":{"outbound":{"A":{"uplink":1}}}}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			core := &countedCore{fakeCore: fakeCore{tags: map[string]bool{"A": true}, states: map[string]*BalancerState{"lb": {Targets: map[string]bool{"A": true}}}}}
			e := &Exporter{Panel: &Panel{URL: srv.URL, Token: "token", Client: srv.Client()}, Core: core, Expvars: &Expvars{URL: srv.URL, Client: srv.Client()}}
			m := e.scrape(context.Background())
			if sampleValues(t, m, "xui_exporter_scrape_success")["source=api;"] != number(failed == "") {
				t.Fatalf("case %s: incorrect API status", failed)
			}
			wantInfo := 1
			if failed == "status" {
				wantInfo = 0
			}
			if len(sampleValues(t, m, "xui_panel_info")) != wantInfo {
				t.Fatal("status data lost or synthesized")
			}
			wantOnlines := 1
			if failed == "onlines" {
				wantOnlines = 0
			}
			if len(sampleValues(t, m, "xui_online_clients")) != wantOnlines {
				t.Fatal("online clients data lost or synthesized")
			}
			wantInbounds := 2
			if failed == "inbounds" {
				wantInbounds = 0
			}
			if len(sampleValues(t, m, "xui_inbound_enabled")) != wantInbounds {
				t.Fatal("inbound data lost or synthesized")
			}
			wantNodes := 1
			if failed == "nodes" {
				wantNodes = 0
			}
			if len(sampleValues(t, m, "xui_node_online")) != wantNodes {
				t.Fatal("node data lost or synthesized")
			}
			if len(sampleValues(t, m, "xui_outbound_traffic_bytes_total")) != 1 {
				t.Fatal("independent traffic lost")
			}
			if failed != "settings" && len(sampleValues(t, m, "xui_balancer_outbound_selected")) != 2 {
				t.Fatal("settings/balancer data lost")
			}
			mu.Lock()
			defer mu.Unlock()
			if counts["/panel/api/xray/"] != 1 || counts["/panel/api/server/status"] != 1 || counts["/panel/api/clients/onlines"] != 1 || counts["/panel/api/inbounds/list"] != 1 || counts["/panel/api/nodes/list"] != 1 || counts["/debug/vars"] != 1 {
				t.Fatal(counts)
			}
			want := 1
			if failed == "settings" {
				want = 0
			}
			if core.lists.Load() != 1 || core.balancers.Load() != int64(want) {
				t.Fatal("unexpected RPC budget", core)
			}
		})
	}
}

func TestStatusUsesSharedDeadlineAndDoesNotRetainOldData(t *testing.T) {
	var mu sync.Mutex
	mode := "healthy"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := mode
		mu.Unlock()
		switch r.URL.Path {
		case "/panel/api/xray/":
			_, _ = w.Write([]byte(`{"success":true,"obj":{"xraySetting":{}}}`))
		case "/panel/api/clients/onlines":
			_, _ = w.Write([]byte(`{"success":true,"obj":[]}`))
		case "/panel/api/inbounds/list":
			_, _ = w.Write([]byte(`{"success":true,"obj":[]}`))
		case "/panel/api/nodes/list":
			_, _ = w.Write([]byte(`{"success":true,"obj":[]}`))
		case "/panel/api/server/status":
			switch current {
			case "healthy":
				_, _ = w.Write([]byte(`{"success":true,"obj":{"panelVersion":"p","xray":{"state":"running"}}}`))
			case "failed":
				w.WriteHeader(503)
			case "timeout":
				<-r.Context().Done()
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	e := &Exporter{Panel: &Panel{URL: srv.URL, Client: srv.Client()}}
	for _, next := range []string{"healthy", "failed", "timeout"} {
		mu.Lock()
		mode = next
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		m := e.scrape(ctx)
		cancel()
		if next == "healthy" {
			if len(sampleValues(t, m, "xui_xray_state")) != 3 {
				t.Fatal("missing healthy states")
			}
		} else {
			if len(sampleValues(t, m, "xui_xray_state")) != 0 || len(sampleValues(t, m, "xui_panel_info")) != 0 {
				t.Fatal("stale status survived failure")
			}
			if sampleValues(t, m, "xui_exporter_scrape_success")["source=api;"] != 0 {
				t.Fatal("failed source marked successful")
			}
		}
	}
}

func TestPanelNodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
		want int
		fail bool
	}{
		{"active nodes", `{"success":true,"obj":[{"id":1,"name":"node1","status":"online"},{"id":2,"name":"node2","status":"offline"}]}`, 200, 2, false},
		{"empty list", `{"success":true,"obj":[]}`, 200, 0, false},
		{"invalid json", `{"success":true,"obj":"not an array"}`, 200, 0, true},
		{"server error", `server down`, 500, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/panel/api/nodes/list" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := &Panel{URL: srv.URL, Token: "token", Client: srv.Client()}
			got, err := p.Nodes(context.Background())
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v, fail=%v", err, tc.fail)
			}
			if len(got) != tc.want {
				t.Fatalf("got=%d, want=%d", len(got), tc.want)
			}
		})
	}
}

func TestNodesDecodesAndMetrics(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raw         string
		wantErr     bool
		wantOnline  map[string]float64
		wantLat     map[string]float64
		wantState   map[string]float64
		wantCPU     map[string]float64
		wantMem     map[string]float64
		wantUptime  map[string]float64
		wantInfo    map[string]float64
		wantClients map[string]float64
	}{
		{
			name: "full node stats",
			raw: `[
				{
					"id": 1,
					"name": "node-us",
					"status": "online",
					"latencyMs": 45,
					"xrayState": "running",
					"cpuPct": 15.5,
					"memPct": 42.1,
					"uptimeSecs": 86400,
					"panelVersion": "3.10.2",
					"xrayVersion": "26.10.10",
					"onlineCount": 7
				},
				{
					"id": 2,
					"name": "node-eu",
					"status": "offline",
					"latencyMs": -1,
					"xrayState": "stop",
					"cpuPct": 0,
					"memPct": 0,
					"uptimeSecs": 0,
					"onlineCount": 3
				}
			]`,
			wantOnline: map[string]float64{"node=node-us;": 1, "node=node-eu;": 0},
			wantLat:    map[string]float64{"node=node-us;": 0.045},
			wantState: map[string]float64{
				"node=node-us;state=running;": 1,
				"node=node-us;state=stop;":    0,
				"node=node-us;state=error;":   0,
				"node=node-eu;state=running;": 0,
				"node=node-eu;state=stop;":    1,
				"node=node-eu;state=error;":   0,
			},
			wantCPU:     map[string]float64{"node=node-us;": 15.5},
			wantMem:     map[string]float64{"node=node-us;": 42.1},
			wantUptime:  map[string]float64{"node=node-us;": 86400},
			wantInfo:    map[string]float64{"node=node-us;panel_version=3.10.2;xray_version=26.10.10;": 1},
			wantClients: map[string]float64{"node=node-us;": 7},
		},
		{
			name:       "empty nodes",
			raw:        `[]`,
			wantOnline: map[string]float64{},
		},
		{
			name:    "missing id",
			raw:     `[{"name":"n1"}]`,
			wantErr: true,
		},
		{
			name:    "missing name",
			raw:     `[{"id":1}]`,
			wantErr: true,
		},
		{
			name:    "empty name",
			raw:     `[{"id":1,"name":"  "}]`,
			wantErr: true,
		},
		{
			name:    "duplicate id",
			raw:     `[{"id":1,"name":"n1"},{"id":1,"name":"n2"}]`,
			wantErr: true,
		},
		{
			name:    "duplicate name",
			raw:     `[{"id":1,"name":"n1"},{"id":2,"name":"n1"}]`,
			wantErr: true,
		},
		{
			name:    "negative online clients",
			raw:     `[{"id":1,"name":"n1","onlineCount":-1}]`,
			wantErr: true,
		},
		{
			name:    "not array",
			raw:     `{}`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := decodeNodes(json.RawMessage(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			m := nodeMetrics(nodes)
			checkMetric := func(metricName string, want map[string]float64) {
				vals := sampleValues(t, m, metricName)
				if len(vals) != len(want) {
					t.Fatalf("%s: got %v, want %v", metricName, vals, want)
				}
				for k, v := range want {
					if vals[k] != v {
						t.Fatalf("%s key %s: got %v, want %v", metricName, k, vals[k], v)
					}
				}
			}
			if tc.wantOnline != nil {
				checkMetric("xui_node_online", tc.wantOnline)
			}
			if tc.wantLat != nil {
				checkMetric("xui_node_latency_seconds", tc.wantLat)
			}
			if tc.wantState != nil {
				checkMetric("xui_node_xray_state", tc.wantState)
			}
			if tc.wantCPU != nil {
				checkMetric("xui_node_cpu_percent", tc.wantCPU)
			}
			if tc.wantMem != nil {
				checkMetric("xui_node_memory_percent", tc.wantMem)
			}
			if tc.wantUptime != nil {
				checkMetric("xui_node_uptime_seconds", tc.wantUptime)
			}
			if tc.wantInfo != nil {
				checkMetric("xui_node_info", tc.wantInfo)
			}
			if tc.wantClients != nil {
				checkMetric("xui_node_online_clients", tc.wantClients)
			}
		})
	}
}

func TestInboundsDecodesAndMetrics(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		nodesByID map[int]string
		wantErr   bool
		want      map[string]float64
		wantBytes map[string]float64
	}{
		{
			name: "active and disabled with default tags and local node",
			raw:  `[{"tag":"vless-443","enable":true,"up":100,"down":200},{"tag":"vmess-80","enable":false}]`,
			want: map[string]float64{
				"inbound=vless-443;node=local;remark=vless-443;": 1,
				"inbound=vmess-80;node=local;remark=vmess-80;":   0,
			},
			wantBytes: map[string]float64{
				"direction=uplink;inbound=vless-443;node=local;remark=vless-443;":   100,
				"direction=downlink;inbound=vless-443;node=local;remark=vless-443;": 200,
			},
		},
		{
			name: "with remarks and node mappings and traffic",
			raw: `[
				{"tag":"n3-in-49785-tcp","remark":"vless-raw-ch","nodeId":3,"enable":true,"up":1048576,"down":52428800},
				{"tag":"n5-in-20338-udp","remark":"awg-unknown","nodeId":5,"enable":true,"up":0,"down":0},
				{"tag":"tun-local","remark":"tun-local","nodeId":0,"enable":true}
			]`,
			nodesByID: map[int]string{3: "ishosting-ch"},
			want: map[string]float64{
				"inbound=n3-in-49785-tcp;node=ishosting-ch;remark=vless-raw-ch;": 1,
				"inbound=n5-in-20338-udp;node=node-5;remark=awg-unknown;":        1,
				"inbound=tun-local;node=local;remark=tun-local;":                 1,
			},
			wantBytes: map[string]float64{
				"direction=uplink;inbound=n3-in-49785-tcp;node=ishosting-ch;remark=vless-raw-ch;":   1048576,
				"direction=downlink;inbound=n3-in-49785-tcp;node=ishosting-ch;remark=vless-raw-ch;": 52428800,
				"direction=uplink;inbound=n5-in-20338-udp;node=node-5;remark=awg-unknown;":          0,
				"direction=downlink;inbound=n5-in-20338-udp;node=node-5;remark=awg-unknown;":        0,
			},
		},
		{
			name: "preserves tag literals with whitespace",
			raw:  `[{"tag":" inbound-spaced ","enable":true},{"tag":"inbound-spaced","enable":false}]`,
			want: map[string]float64{
				"inbound= inbound-spaced ;node=local;remark= inbound-spaced ;": 1,
				"inbound=inbound-spaced;node=local;remark=inbound-spaced;":     0,
			},
		},
		{
			name: "empty array",
			raw:  `[]`,
			want: map[string]float64{},
		},
		{
			name:    "missing tag",
			raw:     `[{"enable":true}]`,
			wantErr: true,
		},
		{
			name:    "empty tag",
			raw:     `[{"tag":" ","enable":true}]`,
			wantErr: true,
		},
		{
			name:    "duplicate tag",
			raw:     `[{"tag":"inb","enable":true},{"tag":"inb","enable":false}]`,
			wantErr: true,
		},
		{
			name:    "missing enable",
			raw:     `[{"tag":"inb"}]`,
			wantErr: true,
		},
		{
			name:    "negative up traffic",
			raw:     `[{"tag":"inb","enable":true,"up":-10}]`,
			wantErr: true,
		},
		{
			name:    "negative down traffic",
			raw:     `[{"tag":"inb","enable":true,"down":-5}]`,
			wantErr: true,
		},
		{
			name:    "not array",
			raw:     `{}`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbounds, err := decodeInbounds(json.RawMessage(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			m := inboundStatusMetrics(inbounds, tc.nodesByID)
			vals := sampleValues(t, m, "xui_inbound_enabled")
			if len(vals) != len(tc.want) {
				t.Fatalf("got %v, want %v", vals, tc.want)
			}
			for k, v := range tc.want {
				if vals[k] != v {
					t.Fatalf("key %s: got %v, want %v", k, vals[k], v)
				}
			}
			if tc.wantBytes != nil {
				byteVals := sampleValues(t, m, "xui_inbound_bytes_total")
				if len(byteVals) != len(tc.wantBytes) {
					t.Fatalf("got %v, want %v", byteVals, tc.wantBytes)
				}
				for k, v := range tc.wantBytes {
					if byteVals[k] != v {
						t.Fatalf("bytes key %s: got %v, want %v", k, byteVals[k], v)
					}
				}
			}
		})
	}
}
