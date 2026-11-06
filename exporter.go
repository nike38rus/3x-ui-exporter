package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	obs "github.com/xtls/xray-core/app/observatory"
	hc "github.com/xtls/xray-core/app/proxyman/command"
	rc "github.com/xtls/xray-core/app/router/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type TrafficCounter struct {
	Name  string
	Value int64
}

type BalancerState struct {
	Targets  map[string]bool
	Override bool
}
type Core interface {
	Outbounds(context.Context) (map[string]bool, error)
	Balancer(context.Context, string) (*BalancerState, error)
}
type GRPCCore struct {
	handlers hc.HandlerServiceClient
	routing  rc.RoutingServiceClient
}

func newCore(conn grpc.ClientConnInterface) *GRPCCore {
	return &GRPCCore{hc.NewHandlerServiceClient(conn), rc.NewRoutingServiceClient(conn)}
}
func (c *GRPCCore) Outbounds(ctx context.Context) (map[string]bool, error) {
	defer reportSlow(time.Now(), "grpc", "ListOutbounds")
	r, e := c.handlers.ListOutbounds(ctx, &hc.ListOutboundsRequest{})
	if e != nil {
		return nil, e
	}
	if r == nil {
		return nil, errors.New("missing outbounds response")
	}
	tags := map[string]bool{}
	for _, o := range r.Outbounds {
		if o.GetTag() == "" || tags[o.GetTag()] {
			return nil, errors.New("empty or duplicate outbound tag")
		}
		tags[o.GetTag()] = true
	}
	return tags, nil
}
func (c *GRPCCore) Balancer(ctx context.Context, tag string) (*BalancerState, error) {
	defer reportSlow(time.Now(), "grpc", "GetBalancerInfo")
	r, e := c.routing.GetBalancerInfo(ctx, &rc.GetBalancerInfoRequest{Tag: tag})
	if e != nil {
		if status.Code(e) == codes.Unknown && status.Convert(e).Message() == "app/router: cannot find tag" {
			return nil, nil
		}
		return nil, e
	}
	if r == nil || r.Balancer == nil {
		return nil, errors.New("missing balancer response")
	}
	tags := map[string]bool{}
	override := false
	if target := r.Balancer.GetOverride().GetTarget(); target != "" {
		tags[target] = true
		override = true
	} else {
		for _, t := range r.Balancer.GetPrincipleTarget().GetTag() {
			if t == "" {
				return nil, errors.New("empty balancer target tag")
			}
			tags[t] = true
		}
	}
	return &BalancerState{Targets: tags, Override: override}, nil
}

// Log only unusually slow operations at warn level, or all operations at debug level.
func reportSlow(start time.Time, source, operation string) {
	elapsed := time.Since(start)
	if elapsed >= 100*time.Millisecond {
		slog.Warn("slow source operation", "source", source, "operation", operation, "duration_seconds", elapsed.Seconds())
	} else {
		slog.Debug("source operation completed", "source", source, "operation", operation, "duration_seconds", elapsed.Seconds())
	}
}

type PanelSource interface {
	Configuration(context.Context) (*Config, error)
	Status(context.Context) (*PanelStatus, error)
}
type ExpvarsSource interface {
	Snapshot(context.Context, *Config) (*ExpvarsSnapshot, error)
}
type explicitExpvars interface {
	hasExplicitURL() bool
}
type Exporter struct {
	Expvars ExpvarsSource
	Panel   PanelSource
	Core    Core
	Timeout time.Duration
	Version string
}
type samples []prometheus.Metric

func (s *samples) add(name, help string, typ prometheus.ValueType, value float64, labels []string, values ...string) {
	*s = append(*s, prometheus.MustNewConstMetric(prometheus.NewDesc(name, help, labels, nil), typ, value, values...))
}
func gauge(s *samples, name, help string, value float64, labels []string, values ...string) {
	s.add(name, help, prometheus.GaugeValue, value, labels, values...)
}
func number(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

type snapshot struct{ metrics samples }

func (s snapshot) Describe(chan<- *prometheus.Desc) {}
func (s snapshot) Collect(ch chan<- prometheus.Metric) {
	for _, m := range s.metrics {
		ch <- m
	}
}

func (e *Exporter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/healthz" {
			_, _ = fmt.Fprintln(w, "ok")
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprint(w, `<h1>3x-ui exporter</h1><a href="/metrics">Metrics</a>`)
			return
		}
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), e.Timeout)
		defer cancel()
		registry := prometheus.NewRegistry()
		registry.MustRegister(
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
			collectors.NewGoCollector(),
			snapshot{e.scrape(ctx)},
		)
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
}
func (e *Exporter) scrape(ctx context.Context) samples {
	apiStart := time.Now()
	var status *PanelStatus
	var statusErr error
	var statusElapsed, onlinesElapsed, inboundsElapsed, nodesElapsed time.Duration
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		defer reportSlow(start, "api", "server/status")
		status, statusErr = e.Panel.Status(ctx)
		if statusErr == nil && status == nil {
			statusErr = errors.New("missing panel status")
		}
		statusElapsed = time.Since(apiStart)
	}()
	var onlines []string
	var onlinesErr error
	var hasOnlines bool
	if onlinesSrc, ok := e.Panel.(OnlineSource); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			defer reportSlow(start, "api", "clients/onlines")
			onlines, onlinesErr = onlinesSrc.Onlines(ctx)
			if onlinesErr == nil {
				hasOnlines = true
			}
			onlinesElapsed = time.Since(apiStart)
		}()
	}
	var inbounds []*InboundStatus
	var inboundsErr error
	var hasInbounds bool
	if inbSrc, ok := e.Panel.(InboundSource); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			defer reportSlow(start, "api", "inbounds/list")
			inbounds, inboundsErr = inbSrc.Inbounds(ctx)
			if inboundsErr == nil {
				hasInbounds = true
			}
			inboundsElapsed = time.Since(apiStart)
		}()
	}
	var nodes []*NodeStatus
	var nodesErr error
	var hasNodes bool
	if nodeSrc, ok := e.Panel.(NodeSource); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			defer reportSlow(start, "api", "nodes/list")
			nodes, nodesErr = nodeSrc.Nodes(ctx)
			if nodesErr == nil {
				hasNodes = true
			}
			nodesElapsed = time.Since(apiStart)
		}()
	}
	var data *ExpvarsSnapshot
	var expErr, grpcErr, balancerErr, trafficErr error
	var expDuration, grpcDuration time.Duration
	var balancer, traffic samples

	hasExplicit := false
	if exp, ok := e.Expvars.(explicitExpvars); ok && exp.hasExplicitURL() {
		hasExplicit = true
	}
	if hasExplicit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			defer reportSlow(start, "expvars", "debug/vars")
			defer func() { expDuration = time.Since(start) }()
			if e.Expvars == nil {
				expErr = errors.New("missing expvars source")
				return
			}
			data, expErr = e.Expvars.Snapshot(ctx, nil)
			if expErr == nil && data == nil {
				expErr = errors.New("missing expvars snapshot")
			}
			if data != nil {
				expErr = errors.Join(expErr, data.Err)
			}
		}()
	}

	cfg, cfgErr := e.Panel.Configuration(ctx)
	configElapsed := time.Since(apiStart)
	reportSlow(apiStart, "api", "getXraySetting")
	if cfgErr == nil && cfg == nil {
		cfgErr = errors.New("missing panel configuration")
	}
	if cfgErr != nil {
		cfg = nil
	}
	if !hasExplicit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			defer reportSlow(start, "expvars", "debug/vars")
			defer func() { expDuration = time.Since(start) }()
			if e.Expvars == nil {
				expErr = errors.New("missing expvars source")
				return
			}
			data, expErr = e.Expvars.Snapshot(ctx, cfg)
			if expErr == nil && data == nil {
				expErr = errors.New("missing expvars snapshot")
			}
			if data != nil {
				expErr = errors.Join(expErr, data.Err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		defer func() { grpcDuration = time.Since(start) }()
		outbounds, states, err := e.readGRPC(ctx, cfg)
		grpcErr = err
		if err == nil && cfg != nil {
			balancer, balancerErr = buildBalancerMetrics(cfg, outbounds, states)
		}
	}()
	wg.Wait()
	apiDuration := max(configElapsed, statusElapsed, onlinesElapsed, inboundsElapsed, nodesElapsed).Seconds()
	if data != nil {
		traffic, trafficErr = trafficMetrics(data.Traffic)
		expErr = errors.Join(expErr, trafficErr)
	}
	result := append(traffic, balancer...)
	result = append(result, panelStatusMetrics(status)...)
	if hasOnlines {
		gauge(&result, "xui_online_clients", "Current active clients online reported by panel.", float64(len(onlines)), nil)
	}
	nodesByID := make(map[int]string)
	if hasNodes {
		for _, n := range nodes {
			nodesByID[n.ID] = n.Name
		}
	}
	if hasInbounds {
		result = append(result, inboundStatusMetrics(inbounds, nodesByID)...)
	}
	if hasNodes {
		result = append(result, nodeMetrics(nodes)...)
	}
	if hasInbounds && hasOnlines {
		localEmails := make(map[string]bool)
		for _, in := range inbounds {
			if in.NodeID == nil || *in.NodeID == 0 {
				for _, email := range in.ClientEmails {
					localEmails[email] = true
				}
			}
		}
		onlineSet := make(map[string]bool, len(onlines))
		for _, email := range onlines {
			onlineSet[email] = true
		}
		localOnlineCount := 0
		for email := range localEmails {
			if onlineSet[email] {
				localOnlineCount++
			}
		}
		gauge(&result, "xui_node_online_clients", "Current active clients online connected to this node reported by panel.", float64(localOnlineCount), []string{"node"}, "local")
	}
	if data != nil {
		result = append(result, data.Memory...)
		result = append(result, data.Observation...)
	}
	for _, item := range []struct {
		name string
		err  error
	}{{"api_settings", cfgErr}, {"api_status", statusErr}, {"api_inbounds", inboundsErr}, {"api_nodes", nodesErr}, {"api_onlines", onlinesErr}, {"expvars", expErr}, {"traffic", trafficErr}, {"grpc", grpcErr}, {"balancer_metrics", balancerErr}} {
		if item.err != nil {
			slog.Warn("collection failed", "operation", item.name, "error", item.err.Error(), "error_type", fmt.Sprintf("%T", item.err))
		} else {
			slog.Debug("collection succeeded", "operation", item.name)
		}
	}
	gauge(&result, "xui_exporter_scrape_success", "Whether source requests succeeded and their responses satisfied the exporter contract.", number(cfgErr == nil && statusErr == nil && inboundsErr == nil && nodesErr == nil && onlinesErr == nil), []string{"source"}, "api")
	gauge(&result, "xui_exporter_scrape_duration_seconds", "Elapsed source collection time in seconds.", apiDuration, []string{"source"}, "api")
	gauge(&result, "xui_exporter_scrape_success", "Whether source requests succeeded and their responses satisfied the exporter contract.", number(expErr == nil), []string{"source"}, "expvars")
	gauge(&result, "xui_exporter_scrape_duration_seconds", "Elapsed source collection time in seconds.", expDuration.Seconds(), []string{"source"}, "expvars")
	gauge(&result, "xui_exporter_scrape_success", "Whether source requests succeeded and their responses satisfied the exporter contract.", number(grpcErr == nil), []string{"source"}, "grpc")
	gauge(&result, "xui_exporter_scrape_duration_seconds", "Elapsed source collection time in seconds.", grpcDuration.Seconds(), []string{"source"}, "grpc")
	gauge(&result, "xui_exporter_build_info", "Exporter build version.", 1, []string{"version"}, e.Version)
	return result
}

// readGRPC probes runtime access even when panel settings are unavailable.
func (e *Exporter) readGRPC(ctx context.Context, cfg *Config) (map[string]bool, map[string]*BalancerState, error) {
	if e.Core == nil {
		return nil, nil, errors.New("missing gRPC source")
	}
	outbounds, err := e.Core.Outbounds(ctx)
	if err != nil {
		return nil, nil, err
	}
	states := map[string]*BalancerState{}
	if cfg != nil && len(cfg.Routing.Balancers) > 0 {
		if len(cfg.Routing.Balancers) == 1 {
			b := cfg.Routing.Balancers[0]
			state, err := e.Core.Balancer(ctx, b.Tag)
			if err != nil {
				return nil, nil, err
			}
			states[b.Tag] = state
		} else {
			var bWg sync.WaitGroup
			var bMu sync.Mutex
			var firstErr error
			for _, b := range cfg.Routing.Balancers {
				bWg.Add(1)
				go func(tag string) {
					defer bWg.Done()
					state, err := e.Core.Balancer(ctx, tag)
					bMu.Lock()
					defer bMu.Unlock()
					if err != nil {
						if firstErr == nil {
							firstErr = err
						}
						return
					}
					states[tag] = state
				}(b.Tag)
			}
			bWg.Wait()
			if firstErr != nil {
				return nil, nil, firstErr
			}
		}
	}
	return outbounds, states, nil
}

func (e *Exporter) balancerMetrics(ctx context.Context, cfg *Config) (samples, error) {
	outbounds, states, err := e.readGRPC(ctx, cfg)
	if err != nil || cfg == nil {
		return nil, err
	}
	return buildBalancerMetrics(cfg, outbounds, states)
}

func buildBalancerMetrics(cfg *Config, outbounds map[string]bool, states map[string]*BalancerState) (samples, error) {
	var result samples
	for _, b := range cfg.Routing.Balancers {
		available, err := resolveAvailable(b.Tag, cfg, states, outbounds, map[string]bool{})
		if err != nil {
			return nil, err
		}
		gauge(&result, "xui_balancer_outbound_available", "Whether a selected target or fallback resolves to an existing runtime outbound; not a health guarantee.", number(available), []string{"balancer"}, b.Tag)
		state := states[b.Tag]
		if state == nil {
			continue
		}
		labels := []string{"balancer", "outbound", "role"}
		const help = "Current primary target membership, configured fallback selection, or override activity."
		gauge(&result, "xui_balancer_outbound_selected", help, number(state.Override), labels, b.Tag, "", "override")
		// Other strategies expose candidates, so their primary/fallback choice is unknown.
		strategy := strings.ToLower(b.Strategy.Type)
		knownSelection := strategy == "leastload" || strategy == "leastping"
		if b.Fallback != "" && (knownSelection || state.Override) {
			gauge(&result, "xui_balancer_outbound_selected", help, number(!state.Override && len(state.Targets) == 0), labels, b.Tag, b.Fallback, "fallback")
		}
		if !knownSelection && !state.Override {
			continue
		}
		tags := map[string]bool{}
		for t := range outbounds {
			for _, prefix := range b.Selector {
				if strings.HasPrefix(t, prefix) {
					tags[t] = true
				}
			}
		}
		if !state.Override {
			for t := range state.Targets {
				tags[t] = true
			}
		}
		ordered := make([]string, 0, len(tags))
		for t := range tags {
			ordered = append(ordered, t)
		}
		sort.Strings(ordered)
		for _, t := range ordered {
			gauge(&result, "xui_balancer_outbound_selected", help, number(!state.Override && state.Targets[t]), labels, b.Tag, t, "primary")
		}
	}
	return result, nil
}
func trafficMetrics(stats []*TrafficCounter) (samples, error) {
	var result samples
	seen := map[string]bool{}
	for _, s := range stats {
		if s == nil {
			return nil, errors.New("nil traffic entry")
		}
		parts := strings.Split(s.Name, ">>>")
		if len(parts) != 4 || (parts[0] != "inbound" && parts[0] != "outbound" && parts[0] != "user") || parts[2] != "traffic" {
			continue
		}
		if parts[1] == "" || (parts[3] != "uplink" && parts[3] != "downlink") || s.Value < 0 || seen[s.Name] {
			return nil, errors.New("invalid or duplicate traffic entry")
		}
		seen[s.Name] = true
		if parts[0] == "user" {
			result.add("xui_client_traffic_bytes_total", "Cumulative Xray client traffic bytes; direction is relative to the proxied connection.", prometheus.CounterValue, float64(s.Value), []string{"client", "direction"}, parts[1], parts[3])
		} else {
			result.add("xui_"+parts[0]+"_traffic_bytes_total", "Cumulative Xray traffic bytes; direction is relative to the proxied connection.", prometheus.CounterValue, float64(s.Value), []string{parts[0], "direction"}, parts[1], parts[3])
		}
	}
	return result, nil
}
func observationMetrics(v *obs.ObservationResult) (samples, error) {
	if v == nil {
		return nil, errors.New("missing observation")
	}
	var result samples
	seen := map[string]bool{}
	for _, s := range v.Status {
		if s == nil || s.OutboundTag == "" || seen[s.OutboundTag] {
			return nil, errors.New("invalid or duplicate observation")
		}
		seen[s.OutboundTag] = true
		labels := []string{"outbound"}
		tag := s.OutboundTag
		gauge(&result, "xui_observatory_outbound_alive", "Xray observer alive state; latest probe for ordinary observer, window state for burst.", number(s.Alive), labels, tag)
		// A failed ordinary probe uses a sentinel delay, not a measurement.
		if s.Alive && s.Delay >= 0 {
			gauge(&result, "xui_observatory_outbound_delay_seconds", "Xray delay field: latest successful probe or burst window average.", float64(s.Delay)/1000, labels, tag)
		}
		h := s.HealthPing
		if h == nil {
			continue
		}
		if h.All < 0 || h.Fail < 0 || h.Fail > h.All {
			return nil, errors.New("invalid probe counts")
		}
		gauge(&result, "xui_observatory_outbound_probe_samples", "Probe count in the current Xray window.", float64(h.All), labels, tag)
		gauge(&result, "xui_observatory_outbound_probe_failed_samples", "Failed probe count in the current Xray window.", float64(h.Fail), labels, tag)
		if h.All == h.Fail {
			continue
		}
		for _, item := range []struct {
			name  string
			value int64
		}{{"mean", h.Average}, {"min", h.Min}, {"max", h.Max}, {"deviation", h.Deviation}} {
			if item.value < 0 {
				return nil, errors.New("invalid RTT")
			}
			gauge(&result, "xui_observatory_outbound_rtt_"+item.name+"_seconds", "Xray burst window RTT statistic in seconds.", float64(item.value)/1e9, labels, tag)
		}
	}
	return result, nil
}
