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
	handlers      hc.HandlerServiceClient
	routing       rc.RoutingServiceClient
	slowThreshold time.Duration
}

func newCore(conn grpc.ClientConnInterface) *GRPCCore {
	return &GRPCCore{handlers: hc.NewHandlerServiceClient(conn), routing: rc.NewRoutingServiceClient(conn)}
}

func (c *GRPCCore) threshold() time.Duration {
	if c == nil || c.slowThreshold <= 0 {
		return 1 * time.Second
	}
	return c.slowThreshold
}

func (c *GRPCCore) Outbounds(ctx context.Context) (map[string]bool, error) {
	defer reportSlow(time.Now(), "grpc", "ListOutbounds", c.threshold())
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
	defer reportSlow(time.Now(), "grpc", "GetBalancerInfo", c.threshold())
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

// Log only unusually slow operations at warn level, or all operations at debug level.
// Threshold defaults to 1/4 of scrape timeout, or 1 second when timeout is not configured.
func reportSlow(start time.Time, source, operation string, threshold time.Duration) {
	if threshold <= 0 {
		threshold = 1 * time.Second
	}
	elapsed := time.Since(start)
	if elapsed >= threshold {
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

func (e *Exporter) slowThreshold() time.Duration {
	if e == nil || e.Timeout <= 0 {
		return 1 * time.Second
	}
	return e.Timeout / 4
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
			_, _ = fmt.Fprint(w, `<h1>Metrics Exporter</h1><a href="/metrics">Metrics</a>`)
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

type configSignal struct {
	done chan struct{}
	cfg  *Config
}

type apiResult struct {
	cfg         *Config
	cfgErr      error
	status      *PanelStatus
	statusErr   error
	onlines     []string
	onlinesErr  error
	hasOnlines  bool
	inbounds    []*InboundStatus
	inboundsErr error
	hasInbounds bool
	nodes       []*NodeStatus
	nodesErr    error
	hasNodes    bool
	duration    time.Duration
}

func (r apiResult) ok() bool {
	return r.cfgErr == nil && r.statusErr == nil && r.inboundsErr == nil && r.nodesErr == nil && r.onlinesErr == nil
}

type expvarsResult struct {
	snapshot *ExpvarsSnapshot
	err      error
	duration time.Duration
}

type grpcResult struct {
	outbounds map[string]bool
	states    map[string]*BalancerState
	err       error
	duration  time.Duration
}

func (e *Exporter) scrape(ctx context.Context) samples {
	sig := &configSignal{done: make(chan struct{})}
	var (
		apiRes  apiResult
		expRes  expvarsResult
		grpcRes grpcResult
		wg      sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		apiRes = e.collectAPI(ctx, sig)
	}()
	go func() {
		defer wg.Done()
		expRes = e.collectExpvars(ctx, sig)
	}()
	go func() {
		defer wg.Done()
		grpcRes = e.collectGRPC(ctx, sig)
	}()
	wg.Wait()

	var observations map[string]bool
	if expRes.snapshot != nil && expRes.snapshot.ObservationsPresent {
		observations = expRes.snapshot.Observations
	}
	var balancer, traffic samples
	var balancerErr, trafficErr error
	if grpcRes.outbounds != nil && apiRes.cfg != nil {
		balancer, balancerErr = buildBalancerMetrics(apiRes.cfg, grpcRes.outbounds, grpcRes.states, observations)
	}
	if expRes.snapshot != nil {
		traffic, trafficErr = trafficMetrics(expRes.snapshot.Traffic)
		expRes.err = errors.Join(expRes.err, trafficErr)
	}

	result := append(traffic, balancer...)
	result = append(result, panelStatusMetrics(apiRes.status)...)
	if apiRes.hasOnlines {
		gauge(&result, "xui_client_online", "Current active client sessions across the cluster reported by panel.", float64(len(apiRes.onlines)), nil)
	}
	nodesByID := make(map[int]string)
	if apiRes.hasNodes {
		for _, n := range apiRes.nodes {
			nodesByID[n.ID] = n.Name
		}
	}
	if apiRes.hasInbounds {
		result = append(result, inboundStatusMetrics(apiRes.inbounds, nodesByID)...)
		result = append(result, clientCountMetrics(apiRes.inbounds, time.Now().UnixMilli())...)
	}
	if apiRes.hasNodes {
		result = append(result, nodeMetrics(apiRes.nodes)...)
	}
	if apiRes.hasInbounds && apiRes.hasOnlines {
		gauge(&result, "xui_node_online_clients", "Current active clients online connected to this node reported by panel.", float64(countLocalOnlineClients(apiRes.inbounds, apiRes.onlines)), []string{"node"}, "local")
	}
	if expRes.snapshot != nil {
		result = append(result, expRes.snapshot.Memory...)
		result = append(result, expRes.snapshot.Observation...)
	}

	for _, item := range []struct {
		name string
		err  error
	}{
		{"api_settings", apiRes.cfgErr},
		{"api_status", apiRes.statusErr},
		{"api_inbounds", apiRes.inboundsErr},
		{"api_nodes", apiRes.nodesErr},
		{"api_onlines", apiRes.onlinesErr},
		{"expvars", expRes.err},
		{"traffic", trafficErr},
		{"grpc", grpcRes.err},
		{"balancer_metrics", balancerErr},
	} {
		if item.err != nil {
			slog.Warn("collection failed", "operation", item.name, "error", item.err.Error(), "error_type", fmt.Sprintf("%T", item.err))
		} else {
			slog.Debug("collection succeeded", "operation", item.name)
		}
	}
	gauge(&result, "xui_exporter_scrape_success", "Whether source requests succeeded and their responses satisfied the exporter contract.", number(apiRes.ok()), []string{"source"}, "api")
	gauge(&result, "xui_exporter_scrape_duration_seconds", "Elapsed source collection time in seconds.", apiRes.duration.Seconds(), []string{"source"}, "api")
	gauge(&result, "xui_exporter_scrape_success", "Whether source requests succeeded and their responses satisfied the exporter contract.", number(expRes.err == nil), []string{"source"}, "expvars")
	gauge(&result, "xui_exporter_scrape_duration_seconds", "Elapsed source collection time in seconds.", expRes.duration.Seconds(), []string{"source"}, "expvars")
	gauge(&result, "xui_exporter_scrape_success", "Whether source requests succeeded and their responses satisfied the exporter contract.", number(grpcRes.err == nil), []string{"source"}, "grpc")
	gauge(&result, "xui_exporter_scrape_duration_seconds", "Elapsed source collection time in seconds.", grpcRes.duration.Seconds(), []string{"source"}, "grpc")
	gauge(&result, "xui_exporter_build_info", "Exporter build version.", 1, []string{"version"}, e.Version)
	return result
}

func countLocalOnlineClients(inbounds []*InboundStatus, onlines []string) int {
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
	count := 0
	for email := range localEmails {
		if onlineSet[email] {
			count++
		}
	}
	return count
}

func (e *Exporter) collectAPI(ctx context.Context, sig *configSignal) apiResult {
	threshold := e.slowThreshold()
	apiStart := time.Now()
	var res apiResult
	var configElapsed, statusElapsed, onlinesElapsed, inboundsElapsed, nodesElapsed time.Duration
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		defer reportSlow(start, "api", "getXraySetting", threshold)
		cfg, err := e.Panel.Configuration(ctx)
		if err == nil && cfg == nil {
			err = errors.New("missing panel configuration")
		}
		if err != nil {
			cfg = nil
		}
		res.cfg = cfg
		res.cfgErr = err
		sig.cfg = cfg
		close(sig.done)
		configElapsed = time.Since(apiStart)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		defer reportSlow(start, "api", "server/status", threshold)
		status, err := e.Panel.Status(ctx)
		if err == nil && status == nil {
			err = errors.New("missing panel status")
		}
		res.status = status
		res.statusErr = err
		statusElapsed = time.Since(apiStart)
	}()

	if onlinesSrc, ok := e.Panel.(OnlineSource); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			defer reportSlow(start, "api", "clients/onlines", threshold)
			onlines, err := onlinesSrc.Onlines(ctx)
			res.onlines = onlines
			res.onlinesErr = err
			res.hasOnlines = err == nil
			onlinesElapsed = time.Since(apiStart)
		}()
	}

	if inbSrc, ok := e.Panel.(InboundSource); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			defer reportSlow(start, "api", "inbounds/list", threshold)
			inbounds, err := inbSrc.Inbounds(ctx)
			res.inbounds = inbounds
			res.inboundsErr = err
			res.hasInbounds = err == nil
			inboundsElapsed = time.Since(apiStart)
		}()
	}

	if nodeSrc, ok := e.Panel.(NodeSource); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			defer reportSlow(start, "api", "nodes/list", threshold)
			nodes, err := nodeSrc.Nodes(ctx)
			res.nodes = nodes
			res.nodesErr = err
			res.hasNodes = err == nil
			nodesElapsed = time.Since(apiStart)
		}()
	}

	wg.Wait()
	res.duration = max(configElapsed, statusElapsed, onlinesElapsed, inboundsElapsed, nodesElapsed)
	return res
}

func (e *Exporter) collectExpvars(ctx context.Context, sig *configSignal) (res expvarsResult) {
	var cfg *Config
	hasExplicit := false
	if exp, ok := e.Expvars.(explicitExpvars); ok && exp.hasExplicitURL() {
		hasExplicit = true
	}
	if !hasExplicit {
		<-sig.done
		cfg = sig.cfg
	}
	start := time.Now()
	defer reportSlow(start, "expvars", "debug/vars", e.slowThreshold())
	defer func() { res.duration = time.Since(start) }()
	if e.Expvars == nil {
		res.err = errors.New("missing expvars source")
		return res
	}
	data, err := e.Expvars.Snapshot(ctx, cfg)
	if err == nil && data == nil {
		err = errors.New("missing expvars snapshot")
	}
	if data != nil {
		err = errors.Join(err, data.Err)
	}
	res.snapshot = data
	res.err = err
	return res
}

func (e *Exporter) collectGRPC(ctx context.Context, sig *configSignal) grpcResult {
	var res grpcResult
	core, err := e.coreWithThreshold()
	if err != nil {
		res.err = err
		return res
	}
	outStart := time.Now()
	outbounds, err := core.Outbounds(ctx)
	res.duration += time.Since(outStart)
	if err != nil {
		res.err = err
		return res
	}
	res.outbounds = outbounds

	<-sig.done
	cfg := sig.cfg
	if cfg == nil || len(cfg.Routing.Balancers) == 0 {
		res.states = map[string]*BalancerState{}
		return res
	}
	balStart := time.Now()
	states, err := readBalancers(ctx, core, cfg.Routing.Balancers)
	res.duration += time.Since(balStart)
	res.states = states
	res.err = err
	return res
}

func (e *Exporter) coreWithThreshold() (Core, error) {
	if e.Core == nil {
		return nil, errors.New("missing gRPC source")
	}
	if c, ok := e.Core.(*GRPCCore); ok && c.slowThreshold <= 0 {
		cp := *c
		cp.slowThreshold = e.slowThreshold()
		return &cp, nil
	}
	return e.Core, nil
}

func readBalancers(ctx context.Context, core Core, balancers []BalancerConfig) (map[string]*BalancerState, error) {
	states := make(map[string]*BalancerState, len(balancers))
	if len(balancers) == 1 {
		b := balancers[0]
		state, err := core.Balancer(ctx, b.Tag)
		if err != nil {
			return states, err
		}
		states[b.Tag] = state
		return states, nil
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, b := range balancers {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			state, err := core.Balancer(ctx, tag)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			states[tag] = state
		}(b.Tag)
	}
	wg.Wait()
	return states, firstErr
}

// readGRPC probes runtime access even when panel settings are unavailable.
func (e *Exporter) readGRPC(ctx context.Context, cfg *Config) (map[string]bool, map[string]*BalancerState, error) {
	core, err := e.coreWithThreshold()
	if err != nil {
		return nil, nil, err
	}
	outbounds, err := core.Outbounds(ctx)
	if err != nil {
		return nil, nil, err
	}
	if cfg == nil || len(cfg.Routing.Balancers) == 0 {
		return outbounds, map[string]*BalancerState{}, nil
	}
	states, err := readBalancers(ctx, core, cfg.Routing.Balancers)
	return outbounds, states, err
}

func (e *Exporter) balancerMetrics(ctx context.Context, cfg *Config) (samples, error) {
	outbounds, states, err := e.readGRPC(ctx, cfg)
	if outbounds == nil || cfg == nil {
		return nil, err
	}
	res, bErr := buildBalancerMetrics(cfg, outbounds, states, nil)
	return res, errors.Join(err, bErr)
}

func buildBalancerMetrics(cfg *Config, outbounds map[string]bool, states map[string]*BalancerState, observations map[string]bool) (samples, error) {
	var result samples
	for _, b := range cfg.Routing.Balancers {
		state, known := states[b.Tag]
		if !known {
			continue
		}
		available, err := resolveAvailable(b.Tag, cfg, states, outbounds, map[string]bool{})
		if err != nil {
			return nil, err
		}
		gauge(&result, "xui_balancer_outbound_available", "Whether a selected target or fallback resolves to an existing runtime outbound; not a health guarantee.", number(available), []string{"balancer"}, b.Tag)
		if state == nil {
			continue
		}
		var balancerState float64
		switch {
		case !available:
			balancerState = 0
		case state.Override:
			balancerState = 3
		case b.Fallback != "" && len(state.Targets) == 0:
			balancerState = 1
		default:
			balancerState = 2
		}
		gauge(&result, "xui_balancer_state", "Current balancer operational state: 0=unavailable, 1=fallback, 2=primary, 3=override.", balancerState, []string{"balancer"}, b.Tag)
		health, hErr := resolveBalancerOperationalHealth(b.Tag, cfg, states, outbounds, observations, map[string]bool{})
		if hErr != nil {
			return nil, hErr
		}
		gauge(&result, "xui_balancer_health", "Current balancer egress operational health: 0=down, 1=blind, 2=up.", float64(health), []string{"balancer"}, b.Tag)
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
		var selectors []string
		for _, prefix := range b.Selector {
			sel := strings.TrimSpace(prefix)
			if sel != "" {
				selectors = append(selectors, sel)
			}
		}
		tags := map[string]bool{}
		for t := range outbounds {
			for _, sel := range selectors {
				if t == sel || strings.HasPrefix(t, sel) {
					tags[t] = true
					break
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
func observationMetrics(v *obs.ObservationResult) (samples, map[string]bool, error) {
	if v == nil {
		return nil, nil, errors.New("missing observation")
	}
	var result samples
	seen := map[string]bool{}
	obsMap := make(map[string]bool, len(v.Status))
	for _, s := range v.Status {
		if s == nil || s.OutboundTag == "" || seen[s.OutboundTag] {
			return nil, nil, errors.New("invalid or duplicate observation")
		}
		seen[s.OutboundTag] = true
		obsMap[s.OutboundTag] = s.Alive
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
			return nil, nil, errors.New("invalid probe counts")
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
				return nil, nil, errors.New("invalid RTT")
			}
			gauge(&result, "xui_observatory_outbound_rtt_"+item.name+"_seconds", "Xray burst window RTT statistic in seconds.", float64(item.value)/1e9, labels, tag)
		}
	}
	return result, obsMap, nil
}
