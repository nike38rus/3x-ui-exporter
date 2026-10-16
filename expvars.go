package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	obs "github.com/xtls/xray-core/app/observatory"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Independent groups survive malformed siblings. Err marks incomplete collection.
type ExpvarsSnapshot struct {
	Traffic             []*TrafficCounter
	TrafficPresent      bool
	Memory, Observation samples
	Err                 error
}
type Expvars struct {
	URL    string
	Client *http.Client
}

func (e *Expvars) Snapshot(ctx context.Context) (*ExpvarsSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(e.URL, "/")+"/debug/vars", nil)
	if err != nil {
		return nil, errors.New("invalid expvars URL")
	}
	// Never forward the panel token to Xray's unauthenticated metrics endpoint.
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, errors.New("expvars request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("expvars HTTP status %d", resp.StatusCode)
	}
	const limit = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return nil, errors.New("invalid or oversized expvars response")
	}
	return decodeExpvars(body)
}
func decodeExpvars(body []byte) (*ExpvarsSnapshot, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		return nil, errors.New("invalid expvars JSON")
	}
	r := &ExpvarsSnapshot{}
	var err error
	if configured(envelope["stats"]) {
		r.Traffic, err = expvarsTraffic(envelope["stats"])
		r.TrafficPresent = err == nil
		r.Err = errors.Join(r.Err, err)
	}
	if configured(envelope["memstats"]) {
		r.Memory, err = expvarsMemory(envelope["memstats"])
		r.Err = errors.Join(r.Err, err)
	}
	if configured(envelope["observatory"]) {
		r.Observation, err = expvarsObservation(envelope["observatory"])
		r.Err = errors.Join(r.Err, err)
	}
	return r, nil
}
func expvarsObservation(raw json.RawMessage) (samples, error) {
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil || entries == nil {
		return nil, errors.New("invalid observatory object")
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := &obs.ObservationResult{}
	for _, key := range keys {
		var entry *obs.OutboundStatus
		if json.Unmarshal(entries[key], &entry) != nil || entry == nil {
			return nil, errors.New("invalid observatory entry")
		}
		// Protobuf JSON omits default scalar values (for example fail=0).
		if entry.OutboundTag == "" {
			entry.OutboundTag = key
		}
		result.Status = append(result.Status, entry)
	}
	return observationMetrics(result)
}
func expvarsTraffic(raw json.RawMessage) ([]*TrafficCounter, error) {
	var groups map[string]json.RawMessage
	if json.Unmarshal(raw, &groups) != nil || groups == nil {
		return nil, errors.New("invalid stats object")
	}
	var result []*TrafficCounter
	for _, kind := range []string{"inbound", "outbound"} {
		if !configured(groups[kind]) {
			continue
		}
		var tags map[string]map[string]*int64
		if json.Unmarshal(groups[kind], &tags) != nil || tags == nil {
			return nil, errors.New("invalid traffic group")
		}
		for tag, counters := range tags {
			if tag == "" || strings.Contains(tag, ">>>") || counters == nil {
				return nil, errors.New("invalid traffic tag")
			}
			for _, direction := range []string{"uplink", "downlink"} {
				value := counters[direction]
				if value == nil {
					continue
				}
				if *value < 0 {
					return nil, errors.New("negative traffic counter")
				}
				result = append(result, &TrafficCounter{Name: kind + ">>>" + tag + ">>>traffic>>>" + direction, Value: *value})
			}
		}
	}
	return result, nil
}
func expvarsMemory(raw json.RawMessage) (samples, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errors.New("invalid memstats object")
	}
	var result samples
	for _, item := range []struct {
		field, name string
		counter     bool
		scale       float64
	}{
		{"Alloc", "memory_alloc_bytes", false, 1},
		{"Sys", "memory_sys_bytes", false, 1},
		{"TotalAlloc", "memory_alloc_bytes_total", true, 1},
		{"Mallocs", "memory_mallocs_total", true, 1},
		{"Frees", "memory_frees_total", true, 1},
		{"HeapObjects", "memory_live_objects", false, 1},
		{"NumGC", "gc_cycles_total", true, 1},
		{"PauseTotalNs", "gc_pause_seconds_total", true, 1e-9},
	} {
		if !configured(fields[item.field]) {
			continue
		}
		var value uint64
		if json.Unmarshal(fields[item.field], &value) != nil {
			return nil, errors.New("invalid memstats value")
		}
		typ := prometheus.GaugeValue
		if item.counter {
			typ = prometheus.CounterValue
		}
		result.add("xui_xray_"+item.name, "Xray Go runtime memory/GC from expvars; memory is not process RSS.", typ, float64(value)*item.scale, nil)
	}
	return result, nil
}
