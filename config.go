package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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
		return nil, panelHTTPError(resp)
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

func panelHTTPError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("panel HTTP status 401: unauthorized (check XUI_API_TOKEN)")
	case http.StatusForbidden:
		return fmt.Errorf("panel HTTP status 403: Administrator scope required")
	default:
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			if loc := resp.Header.Get("Location"); loc != "" {
				return fmt.Errorf("panel HTTP status %d: redirected to %s", resp.StatusCode, loc)
			}
			return fmt.Errorf("panel HTTP status %d: redirected", resp.StatusCode)
		}
		return fmt.Errorf("panel HTTP status %d", resp.StatusCode)
	}
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

// resolveBalancerOperationalHealth returns 2 (up), 1 (blind), or 0 (down).
func resolveBalancerOperationalHealth(tag string, cfg *Config, states map[string]*BalancerState, outbounds map[string]bool, observations map[string]bool, visiting map[string]bool) (int, error) {
	key := "balancer:" + tag
	if visiting[key] {
		return 0, nil
	}
	visiting[key] = true
	defer delete(visiting, key)

	state, known := states[tag]
	if !known || state == nil {
		return 0, nil
	}

	available, err := resolveAvailable(tag, cfg, states, outbounds, map[string]bool{})
	if err != nil {
		return 0, err
	}
	if !available {
		return 0, nil
	}

	var targets []string
	var currentBalancer *BalancerConfig
	for i := range cfg.Routing.Balancers {
		if cfg.Routing.Balancers[i].Tag == tag {
			currentBalancer = &cfg.Routing.Balancers[i]
			break
		}
	}

	if state.Override {
		for t := range state.Targets {
			targets = append(targets, t)
		}
	} else if currentBalancer != nil && currentBalancer.Fallback != "" && len(state.Targets) == 0 {
		targets = append(targets, currentBalancer.Fallback)
	} else {
		for t := range state.Targets {
			targets = append(targets, t)
		}
	}

	if len(targets) == 0 {
		return 0, nil
	}

	sort.Strings(targets)
	maxHealth := 0
	for _, target := range targets {
		health, err := resolveOutboundHealth(target, cfg, states, outbounds, observations, visiting)
		if err != nil {
			return 0, err
		}
		if health > maxHealth {
			maxHealth = health
		}
		if maxHealth == 2 {
			break
		}
	}

	return maxHealth, nil
}

func resolveOutboundHealth(tag string, cfg *Config, states map[string]*BalancerState, outbounds map[string]bool, observations map[string]bool, visiting map[string]bool) (int, error) {
	if tag == "" || !outbounds[tag] {
		return 0, nil
	}
	key := "outbound:" + tag
	if visiting[key] {
		return 0, nil
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
					return 0, errors.New("conditional loopback cannot be resolved")
				}
			}
			var target, balancer string
			_ = json.Unmarshal(r["outboundTag"], &target)
			_ = json.Unmarshal(r["balancerTag"], &balancer)
			if target != "" {
				return resolveOutboundHealth(target, cfg, states, outbounds, observations, visiting)
			}
			if balancer != "" {
				return resolveBalancerOperationalHealth(balancer, cfg, states, outbounds, observations, visiting)
			}
			return 0, nil
		}
		return 0, nil
	}

	// Terminal physical outbound reached.
	if observations != nil {
		if alive, ok := observations[tag]; ok {
			if alive {
				return 2, nil
			}
			return 0, nil
		}
	}
	return 1, nil // unmonitored / unprobed -> BLIND
}
