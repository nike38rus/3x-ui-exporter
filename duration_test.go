package main

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

type delayedPanel struct {
	cfg   *Config
	delay time.Duration
}

func (p delayedPanel) Configuration(context.Context) (*Config, error) {
	time.Sleep(p.delay)
	return p.cfg, nil
}

type delayedObserver struct{ delay time.Duration }

func (o delayedObserver) Snapshot(context.Context) (*ExpvarsSnapshot, error) {
	time.Sleep(o.delay)
	return &ExpvarsSnapshot{TrafficPresent: true}, nil
}

type delayedCore struct{ fakeCore }

func (c *delayedCore) Outbounds(ctx context.Context) (map[string]bool, error) {
	time.Sleep(10 * time.Millisecond)
	return c.fakeCore.Outbounds(ctx)
}
func (c *delayedCore) Balancer(ctx context.Context, tag string) (*BalancerState, error) {
	time.Sleep(20 * time.Millisecond)
	return c.fakeCore.Balancer(ctx, tag)
}

func TestSourceDurationsExcludeOtherSources(t *testing.T) {
	for _, tc := range []struct {
		name         string
		api, expvars time.Duration
	}{
		{"fast dependencies", 0, 0},
		{"slow API", 200 * time.Millisecond, 0},
		{"slow expvars", 0, 300 * time.Millisecond},
		{"both slow", 200 * time.Millisecond, 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := configuration(t, `{"observatory":{},"metrics":{"listen":"127.0.0.1:11111"},"routing":{"balancers":[{"tag":"lb","selector":["A"],"strategy":{"type":"leastLoad"}}]}}`)
				core := &delayedCore{fakeCore{tags: map[string]bool{"A": true}, states: map[string]*BalancerState{"lb": {Targets: map[string]bool{"A": true}}}}}
				e := &Exporter{Panel: delayedPanel{cfg, tc.api}, Core: core, Expvars: delayedObserver{tc.expvars}}
				m := e.scrape(context.Background())
				values := sampleValues(t, m, "xui_exporter_scrape_duration_seconds")
				// Only outbounds+balancer: 10+20; HTTP supplies traffic.
				for source, want := range map[string]float64{"grpc": .030, "api": tc.api.Seconds(), "expvars": tc.expvars.Seconds()} {
					if got := values["source="+source+";"]; got != want {
						t.Errorf("%s duration=%v, want %v", source, got, want)
					}
				}
			})
		})
	}
}
