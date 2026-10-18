package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type BalancerConfig struct {
	Tag      string   `json:"tag"`
	Selector []string `json:"selector"`
	Fallback string   `json:"fallbackTag"`
	Strategy struct {
		Type string `json:"type"`
	} `json:"strategy"`
}
type OutboundConfig struct {
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"`
	Settings struct {
		InboundTag string `json:"inboundTag"`
	} `json:"settings"`
}
type Config struct {
	Metrics struct {
		Listen string `json:"listen"`
	} `json:"metrics"`
	API struct {
		Services []string `json:"services"`
	} `json:"api"`
	Routing struct {
		Balancers []BalancerConfig             `json:"balancers"`
		Rules     []map[string]json.RawMessage `json:"rules"`
	} `json:"routing"`
	Outbounds        []OutboundConfig `json:"outbounds"`
	Observatory      json.RawMessage  `json:"observatory"`
	BurstObservatory json.RawMessage  `json:"burstObservatory"`
}

func (c *Config) observed() bool {
	return configured(c.Observatory) || configured(c.BurstObservatory)
}
func (c *Config) metricsEnabled() bool {
	return strings.TrimSpace(c.Metrics.Listen) != ""
}
func configured(v json.RawMessage) bool { return len(v) > 0 && string(v) != "null" }

type Panel struct {
	URL, Token string
	Client     *http.Client
}

func (p *Panel) object(ctx context.Context, method, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.URL, "/")+path, nil)
	if err != nil {
		return nil, errors.New("invalid panel URL")
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, errors.New("panel request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("panel HTTP status %d", resp.StatusCode)
	}
	const limit = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return nil, errors.New("invalid or oversized panel response")
	}
	var envelope struct {
		Success bool            `json:"success"`
		Object  json.RawMessage `json:"obj"`
	}
	if json.Unmarshal(body, &envelope) != nil || !envelope.Success || !configured(envelope.Object) {
		return nil, errors.New("panel response unsuccessful")
	}
	return envelope.Object, nil
}

func (p *Panel) Configuration(ctx context.Context) (*Config, error) {
	object, err := p.object(ctx, http.MethodPost, "/panel/api/xray/")
	if err != nil {
		return nil, err
	}

	raw := object
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, errors.New("invalid encoded configuration")
		}
		raw = []byte(s)
	}
	if len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New("configuration must be an object")
	}
	var settings struct {
		Template              json.RawMessage  `json:"xraySetting"`
		SubscriptionOutbounds []OutboundConfig `json:"subscriptionOutbounds"`
	}
	if json.Unmarshal(raw, &settings) != nil || !configured(settings.Template) || settings.Template[0] != '{' {
		return nil, errors.New("invalid Xray settings response")
	}
	var cfg Config
	if json.Unmarshal(settings.Template, &cfg) != nil {
		return nil, errors.New("invalid configuration JSON")
	}
	cfg.Outbounds = append(cfg.Outbounds, settings.SubscriptionOutbounds...)
	activeRules := cfg.Routing.Rules[:0]
	for _, rule := range cfg.Routing.Rules {
		var enabled bool
		var outbound string
		_ = json.Unmarshal(rule["outboundTag"], &outbound)
		var inbound []string
		_ = json.Unmarshal(rule["inboundTag"], &inbound)
		apiRule := false
		for _, tag := range inbound {
			apiRule = apiRule || (tag == "api" && outbound == "api")
		}
		var singleInbound string
		if json.Unmarshal(rule["inboundTag"], &singleInbound) == nil {
			apiRule = singleInbound == "api" && outbound == "api"
		}
		if value, exists := rule["enabled"]; exists && json.Unmarshal(value, &enabled) == nil && !enabled && !apiRule {
			continue
		}
		delete(rule, "enabled")
		delete(rule, "comment")
		activeRules = append(activeRules, rule)
	}
	cfg.Routing.Rules = activeRules
	seen := map[string]bool{}
	for _, b := range cfg.Routing.Balancers {
		if b.Tag == "" || seen[b.Tag] || len(b.Selector) == 0 {
			return nil, errors.New("invalid or duplicate balancer configuration")
		}
		seen[b.Tag] = true
	}
	return &cfg, nil
}
func validPanelURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// resolveAvailable follows only unconditional loopback rules. It never probes a channel.
func resolveAvailable(tag string, cfg *Config, states map[string]*BalancerState, outbounds map[string]bool, visiting map[string]bool) (bool, error) {
	key := "balancer:" + tag
	if visiting[key] {
		return false, nil
	}
	visiting[key] = true
	defer delete(visiting, key)
	state, known := states[tag]
	if !known || state == nil {
		return false, nil
	}
	if len(state.Targets) > 0 {
		for target := range state.Targets {
			ok, err := resolveOutbound(target, cfg, states, outbounds, visiting)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	for _, b := range cfg.Routing.Balancers {
		if b.Tag == tag {
			return resolveOutbound(b.Fallback, cfg, states, outbounds, visiting)
		}
	}
	return false, nil
}
func resolveOutbound(tag string, cfg *Config, states map[string]*BalancerState, outbounds map[string]bool, visiting map[string]bool) (bool, error) {
	if tag == "" || !outbounds[tag] {
		return false, nil
	}
	key := "outbound:" + tag
	if visiting[key] {
		return false, nil
	}
	visiting[key] = true
	defer delete(visiting, key)
	for _, o := range cfg.Outbounds {
		if o.Tag != tag || o.Protocol != "loopback" {
			continue
		}
		for _, r := range cfg.Routing.Rules {
			var inbound []string
			if json.Unmarshal(r["inboundTag"], &inbound) != nil {
				continue
			}
			matched := false
			for _, v := range inbound {
				if v == o.Settings.InboundTag && v != "" {
					matched = true
				}
			}
			if !matched {
				continue
			}
			for k := range r {
				if k != "type" && k != "inboundTag" && k != "outboundTag" && k != "balancerTag" && k != "ruleTag" {
					return false, errors.New("conditional loopback cannot be resolved")
				}
			}
			var target, balancer string
			_ = json.Unmarshal(r["outboundTag"], &target)
			_ = json.Unmarshal(r["balancerTag"], &balancer)
			if target != "" {
				return resolveOutbound(target, cfg, states, outbounds, visiting)
			}
			if balancer != "" {
				return resolveAvailable(balancer, cfg, states, outbounds, visiting)
			}
			return false, nil
		}
		return false, nil
	}
	return true, nil
}
