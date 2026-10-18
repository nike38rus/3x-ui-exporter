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
		if o.GetTag() == "" {
			return nil, errors.New("empty outbound tag")
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
				continue
			}
			tags[t] = true
		}
	}
	return &BalancerState{Targets: tags, Override: override}, nil
}

// Log only unusually slow operations, without tokens, configuration or targets.
func reportSlow(start time.Time, source, operation string) {
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		slog.Warn("slow source operation", "source", source, "operation", operation, "duration_seconds", elapsed.Seconds())
	}
}

type ConfigSource interface {
	Configuration(context.Context) (*Config, error)
}
type ExpvarsSource interface {
	Snapshot(context.Context) (*ExpvarsSnapshot, error)
}
type Exporter struct {
	Expvars ExpvarsSource
	Panel   ConfigSource
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
		registry.MustRegister(snapshot{e.scrape(ctx)})
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
}
func (e *Exporter) scrape(ctx context.Context) samples {
	apiStart := time.Now()
	cfg, cfgErr := e.Panel.Configuration(ctx)
	apiDuration := time.Since(apiStart).Seconds()
	reportSlow(apiStart, "api", "getXraySetting")
	expvarsEnabled := cfg == nil || cfg.metricsEnabled()
	grpcEnabled := cfg != nil && len(cfg.Routing.Balancers) > 0
	var data *ExpvarsSnapshot
	var expErr, balancerErr, trafficErr error
	var expDuration, grpcDuration time.Duration
	var balancer, traffic samples
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if !expvarsEnabled {
			return
		}
		start := time.Now()
		defer reportSlow(start, "expvars", "debug/vars")
		defer func() { expDuration = time.Since(start) }()
		if e.Expvars == nil {
			expErr = errors.New("missing expvars source")
			return
		}
		data, expErr = e.Expvars.Snapshot(ctx)
		if expErr == nil && data == nil {
			expErr = errors.New("missing expvars snapshot")
		}
		if data != nil {
			expErr = errors.Join(expErr, data.Err)
		}
	}()
	go func() {
		defer wg.Done()
		if !grpcEnabled {
			return
		}
		start := time.Now()
		defer func() { grpcDuration = time.Since(start) }()
		balancer, balancerErr = e.balancerMetrics(ctx, cfg)
	}()
	wg.Wait()
	if data != nil {
		traffic, trafficErr = trafficMetrics(data.Traffic)
		expErr = errors.Join(expErr, trafficErr)
	}
	result := append(traffic, balancer...)
	if data != nil {
		result = append(result, data.Memory...)
		result = append(result, data.Observation...)
	}
	for _, item := range []struct {
		name string
		err  error
	}{{"api", cfgErr}, {"expvars", expErr}, {"traffic", trafficErr}, {"balancer", balancerErr}} {
		if item.err != nil {
			slog.Warn("collection failed", "operation", item.name, "error_type", fmt.Sprintf("%T", item.err))
		}
	}
	gauge(&result, "xui_exporter_scrape_success", "Whether all required operations for this source succeeded.", number(cfgErr == nil), []string{"source"}, "api")
	gauge(&result, "xui_exporter_scrape_duration_seconds", "Elapsed source collection time in seconds.", apiDuration, []string{"source"}, "api")
	if expvarsEnabled {
		gauge(&result, "xui_exporter_scrape_success", "Whether all required operations for this source succeeded.", number(expErr == nil), []string{"source"}, "expvars")
		gauge(&result, "xui_exporter_scrape_duration_seconds", "Elapsed source collection time in seconds.", expDuration.Seconds(), []string{"source"}, "expvars")
	}
	if grpcEnabled {
		gauge(&result, "xui_exporter_scrape_success", "Whether all required operations for this source succeeded.", number(balancerErr == nil), []string{"source"}, "grpc")
		gauge(&result, "xui_exporter_scrape_duration_seconds", "Elapsed source collection time in seconds.", grpcDuration.Seconds(), []string{"source"}, "grpc")
	}
	gauge(&result, "xui_exporter_build_info", "Exporter build version.", 1, []string{"version"}, e.Version)
	return result
}
func (e *Exporter) balancerMetrics(ctx context.Context, cfg *Config) (samples, error) {
	var result samples
	if len(cfg.Routing.Balancers) == 0 {
		return result, nil
	}
	outbounds, err := e.Core.Outbounds(ctx)
	if err != nil {
		return nil, err
	}
	states := map[string]*BalancerState{}
	for _, b := range cfg.Routing.Balancers {
		s, err := e.Core.Balancer(ctx, b.Tag)
		if err != nil {
			return nil, err
		}
		states[b.Tag] = s
	}
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
		if len(parts) != 4 || (parts[0] != "inbound" && parts[0] != "outbound") || parts[2] != "traffic" {
			continue
		}
		if parts[1] == "" || (parts[3] != "uplink" && parts[3] != "downlink") || s.Value < 0 || seen[s.Name] {
			return nil, errors.New("invalid or duplicate traffic entry")
		}
		seen[s.Name] = true
		result.add("xui_"+parts[0]+"_traffic_bytes_total", "Cumulative Xray traffic bytes; direction is relative to the proxied connection.", prometheus.CounterValue, float64(s.Value), []string{parts[0], "direction"}, parts[1], parts[3])
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
