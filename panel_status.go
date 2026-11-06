package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// PanelStatus is the panel's snapshot, not a direct core health check.
// Do not retain errorMsg, host information or other unrelated status fields.
type PanelStatus struct {
	PanelVersion string
	XrayVersion  string
	XrayState    string
	TCPCount     *int64
	UDPCount     *int64
}

type OnlineSource interface {
	Onlines(context.Context) ([]string, error)
}

type ClientStatus struct {
	Email      string
	Enable     bool
	Up         int64
	Down       int64
	ExpiryTime int64
	Total      int64
}

type InboundStatus struct {
	Tag          string
	Remark       string
	NodeID       *int
	Enable       bool
	Up           *int64
	Down         *int64
	ClientEmails []string
	Clients      []*ClientStatus
}

type InboundSource interface {
	Inbounds(context.Context) ([]*InboundStatus, error)
}

type NodeStatus struct {
	ID           int
	Name         string
	Status       string
	LatencyMs    int
	XrayState    string
	CPUPct       *float64
	MemPct       *float64
	UptimeSecs   *uint64
	PanelVersion string
	XrayVersion  string
	OnlineCount  *int
}

type NodeSource interface {
	Nodes(context.Context) ([]*NodeStatus, error)
}

func (p *Panel) Status(ctx context.Context) (*PanelStatus, error) {
	raw, err := p.object(ctx, http.MethodGet, "/panel/api/server/status")
	if err != nil {
		return nil, err
	}
	return decodePanelStatus(raw)
}

func (p *Panel) Onlines(ctx context.Context) ([]string, error) {
	raw, err := p.object(ctx, http.MethodPost, "/panel/api/clients/onlines")
	if err != nil {
		return nil, err
	}
	return decodeOnlines(raw)
}

func (p *Panel) Inbounds(ctx context.Context) ([]*InboundStatus, error) {
	raw, err := p.object(ctx, http.MethodGet, "/panel/api/inbounds/list")
	if err != nil {
		return nil, err
	}
	return decodeInbounds(raw)
}

func (p *Panel) Nodes(ctx context.Context) ([]*NodeStatus, error) {
	raw, err := p.object(ctx, http.MethodGet, "/panel/api/nodes/list")
	if err != nil {
		return nil, err
	}
	return decodeNodes(raw)
}

func decodeNodes(raw json.RawMessage) ([]*NodeStatus, error) {
	type rawNode struct {
		ID           *int     `json:"id"`
		Name         *string  `json:"name"`
		Status       *string  `json:"status"`
		LatencyMs    *int     `json:"latencyMs"`
		XrayState    *string  `json:"xrayState"`
		CPUPct       *float64 `json:"cpuPct"`
		MemPct       *float64 `json:"memPct"`
		UptimeSecs   *uint64  `json:"uptimeSecs"`
		PanelVersion *string  `json:"panelVersion"`
		XrayVersion  *string  `json:"xrayVersion"`
		OnlineCount  *int     `json:"onlineCount"`
	}
	var list []rawNode
	if json.Unmarshal(raw, &list) != nil || list == nil {
		return nil, errors.New("invalid nodes array")
	}
	seenID := map[int]bool{}
	seenName := map[string]bool{}
	var result []*NodeStatus
	for _, item := range list {
		if item.ID == nil || item.Name == nil {
			return nil, errors.New("invalid node entry")
		}
		name := strings.TrimSpace(*item.Name)
		if name == "" || seenID[*item.ID] || seenName[name] {
			return nil, errors.New("empty, invalid or duplicate node")
		}
		if item.OnlineCount != nil && *item.OnlineCount < 0 {
			return nil, errors.New("negative node online clients")
		}
		if item.Status == nil || strings.TrimSpace(*item.Status) == "" {
			return nil, errors.New("empty, invalid or missing node status")
		}
		seenID[*item.ID] = true
		seenName[name] = true

		s := &NodeStatus{
			ID:          *item.ID,
			Name:        name,
			Status:      strings.TrimSpace(*item.Status),
			CPUPct:      item.CPUPct,
			MemPct:      item.MemPct,
			OnlineCount: item.OnlineCount,
		}
		if item.LatencyMs != nil {
			s.LatencyMs = *item.LatencyMs
		}
		if item.XrayState != nil {
			s.XrayState = strings.TrimSpace(*item.XrayState)
		}
		if item.UptimeSecs != nil {
			s.UptimeSecs = item.UptimeSecs
		}
		if item.PanelVersion != nil {
			s.PanelVersion = strings.TrimSpace(*item.PanelVersion)
		}
		if item.XrayVersion != nil {
			s.XrayVersion = strings.TrimSpace(*item.XrayVersion)
		}
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func decodeInbounds(raw json.RawMessage) ([]*InboundStatus, error) {
	type rawClientTraffic struct {
		Email      *string `json:"email"`
		Enable     *bool   `json:"enable"`
		Up         *int64  `json:"up"`
		Down       *int64  `json:"down"`
		ExpiryTime *int64  `json:"expiryTime"`
		Total      *int64  `json:"total"`
	}
	type rawInbound struct {
		Tag         *string            `json:"tag"`
		Remark      *string            `json:"remark"`
		NodeID      *int               `json:"nodeId"`
		Enable      *bool              `json:"enable"`
		Up          *int64             `json:"up"`
		Down        *int64             `json:"down"`
		ClientStats []rawClientTraffic `json:"clientStats"`
	}
	var list []rawInbound
	if json.Unmarshal(raw, &list) != nil || list == nil {
		return nil, errors.New("invalid inbounds array")
	}
	seen := map[string]bool{}
	var result []*InboundStatus
	for _, item := range list {
		if item.Tag == nil || item.Enable == nil {
			return nil, errors.New("invalid inbound entry")
		}
		tag := *item.Tag
		if strings.TrimSpace(tag) == "" || seen[tag] {
			return nil, errors.New("empty or duplicate inbound tag")
		}
		if item.Up != nil && *item.Up < 0 {
			return nil, errors.New("negative inbound up traffic")
		}
		if item.Down != nil && *item.Down < 0 {
			return nil, errors.New("negative inbound down traffic")
		}
		seen[tag] = true
		remark := ""
		if item.Remark != nil {
			remark = strings.TrimSpace(*item.Remark)
		}
		var emails []string
		var clients []*ClientStatus
		for _, c := range item.ClientStats {
			if c.Up != nil && *c.Up < 0 {
				return nil, errors.New("negative client up traffic")
			}
			if c.Down != nil && *c.Down < 0 {
				return nil, errors.New("negative client down traffic")
			}
			if c.Total != nil && *c.Total < 0 {
				return nil, errors.New("negative client total traffic")
			}
			if c.Email != nil {
				e := strings.TrimSpace(*c.Email)
				if e != "" {
					emails = append(emails, e)
					cs := &ClientStatus{
						Email:  e,
						Enable: true,
					}
					if c.Enable != nil {
						cs.Enable = *c.Enable
					}
					if c.Up != nil {
						cs.Up = *c.Up
					}
					if c.Down != nil {
						cs.Down = *c.Down
					}
					if c.ExpiryTime != nil {
						cs.ExpiryTime = *c.ExpiryTime
					}
					if c.Total != nil {
						cs.Total = *c.Total
					}
					clients = append(clients, cs)
				}
			}
		}
		result = append(result, &InboundStatus{
			Tag:          tag,
			Remark:       remark,
			NodeID:       item.NodeID,
			Enable:       *item.Enable,
			Up:           item.Up,
			Down:         item.Down,
			ClientEmails: emails,
			Clients:      clients,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Tag < result[j].Tag
	})
	return result, nil
}

func clientCountMetrics(inbounds []*InboundStatus, nowMillis int64) samples {
	type clientState struct {
		enable     bool
		up         int64
		down       int64
		expiryTime int64
		total      int64
	}
	uniqueClients := make(map[string]clientState)
	for _, in := range inbounds {
		for _, c := range in.Clients {
			if c == nil || c.Email == "" {
				continue
			}
			if existing, exists := uniqueClients[c.Email]; exists {
				if !c.Enable {
					existing.enable = false
				}
				if c.Up > existing.up {
					existing.up = c.Up
				}
				if c.Down > existing.down {
					existing.down = c.Down
				}
				if c.Total > existing.total {
					existing.total = c.Total
				}
				if c.ExpiryTime > existing.expiryTime {
					existing.expiryTime = c.ExpiryTime
				}
				uniqueClients[c.Email] = existing
			} else {
				uniqueClients[c.Email] = clientState{
					enable:     c.Enable,
					up:         c.Up,
					down:       c.Down,
					expiryTime: c.ExpiryTime,
					total:      c.Total,
				}
			}
		}
	}

	counts := map[string]int{
		"active":   0,
		"depleted": 0,
		"disabled": 0,
		"expired":  0,
	}

	for _, c := range uniqueClients {
		switch {
		case !c.enable:
			counts["disabled"]++
		case c.expiryTime > 0 && c.expiryTime <= nowMillis:
			counts["expired"]++
		case c.total > 0 && (c.up+c.down) >= c.total:
			counts["depleted"]++
		default:
			counts["active"]++
		}
	}

	var result samples
	states := []string{"active", "depleted", "disabled", "expired"}
	for _, state := range states {
		gauge(&result, "xui_client_count", "Total configured client accounts in the panel database by lifecycle state.", float64(counts[state]), []string{"state"}, state)
	}
	return result
}

func nodeMetrics(nodes []*NodeStatus) samples {
	var result samples
	for _, n := range nodes {
		labels := []string{"node"}
		if n.Status == "online" {
			gauge(&result, "xui_node_online", "Whether node is reported online by master panel heartbeat.", 1, labels, n.Name)
		} else if n.Status == "offline" {
			gauge(&result, "xui_node_online", "Whether node is reported online by master panel heartbeat.", 0, labels, n.Name)
		}
		isOnline := n.Status == "online"
		if isOnline && n.LatencyMs >= 0 {
			gauge(&result, "xui_node_latency_seconds", "Round-trip latency between master panel and node in seconds.", float64(n.LatencyMs)/1000, labels, n.Name)
		}
		if n.XrayState != "" {
			for _, state := range []string{"running", "stop", "error"} {
				gauge(&result, "xui_node_xray_state", "Remote node Xray core state captured during heartbeat.", number(n.XrayState == state), []string{"node", "state"}, n.Name, state)
			}
		}
		if isOnline && n.CPUPct != nil && *n.CPUPct >= 0 {
			gauge(&result, "xui_node_cpu_percent", "Remote node CPU utilization percent.", *n.CPUPct, labels, n.Name)
		}
		if isOnline && n.MemPct != nil && *n.MemPct >= 0 {
			gauge(&result, "xui_node_memory_percent", "Remote node memory utilization percent.", *n.MemPct, labels, n.Name)
		}
		if isOnline && n.UptimeSecs != nil {
			gauge(&result, "xui_node_uptime_seconds", "Remote node uptime in seconds.", float64(*n.UptimeSecs), labels, n.Name)
		}
		if isOnline && n.OnlineCount != nil && *n.OnlineCount >= 0 {
			gauge(&result, "xui_node_online_clients", "Current active clients online connected to this node reported by panel.", float64(*n.OnlineCount), labels, n.Name)
		}
		if isOnline && (n.PanelVersion != "" || n.XrayVersion != "") {
			gauge(&result, "xui_node_info", "Remote node version metadata.", 1, []string{"node", "xray_version", "panel_version"}, n.Name, n.XrayVersion, n.PanelVersion)
		}
	}
	return result
}

func inboundStatusMetrics(inbounds []*InboundStatus, nodesByID map[int]string) samples {
	var result samples
	for _, in := range inbounds {
		nodeName := "local"
		if in.NodeID != nil && *in.NodeID != 0 {
			if name, ok := nodesByID[*in.NodeID]; ok && name != "" {
				nodeName = name
			} else {
				nodeName = fmt.Sprintf("node-%d", *in.NodeID)
			}
		}
		remark := in.Remark
		if remark == "" {
			remark = in.Tag
		}
		gauge(&result, "xui_inbound_enabled", "Whether inbound is administratively enabled in the panel.", number(in.Enable), []string{"inbound", "remark", "node"}, in.Tag, remark, nodeName)
		if in.Up != nil && *in.Up >= 0 {
			result.add("xui_inbound_bytes_total", "Cumulative inbound traffic bytes reported by panel; direction is relative to the proxied connection.", prometheus.CounterValue, float64(*in.Up), []string{"inbound", "remark", "node", "direction"}, in.Tag, remark, nodeName, "uplink")
		}
		if in.Down != nil && *in.Down >= 0 {
			result.add("xui_inbound_bytes_total", "Cumulative inbound traffic bytes reported by panel; direction is relative to the proxied connection.", prometheus.CounterValue, float64(*in.Down), []string{"inbound", "remark", "node", "direction"}, in.Tag, remark, nodeName, "downlink")
		}
	}
	return result
}

func decodeOnlines(raw json.RawMessage) ([]string, error) {
	var onlines []string
	if json.Unmarshal(raw, &onlines) != nil || onlines == nil {
		return nil, errors.New("invalid onlines array")
	}
	return onlines, nil
}

func decodePanelStatus(raw json.RawMessage) (*PanelStatus, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errors.New("invalid panel status object")
	}
	s := &PanelStatus{}
	var decodeErr error
	readString := func(v json.RawMessage, dest *string) {
		if !configured(v) {
			return
		}
		var value string
		if json.Unmarshal(v, &value) != nil {
			decodeErr = errors.Join(decodeErr, errors.New("invalid status field"))
			return
		}
		*dest = strings.TrimSpace(value)
	}
	readInt := func(v json.RawMessage, dest **int64) {
		if !configured(v) {
			return
		}
		var n int64
		if json.Unmarshal(v, &n) != nil {
			decodeErr = errors.Join(decodeErr, errors.New("invalid int status field"))
			return
		}
		*dest = &n
	}
	readString(fields["panelVersion"], &s.PanelVersion)
	readInt(fields["tcpCount"], &s.TCPCount)
	readInt(fields["udpCount"], &s.UDPCount)
	if configured(fields["xray"]) {
		var core map[string]json.RawMessage
		if json.Unmarshal(fields["xray"], &core) != nil || core == nil {
			decodeErr = errors.Join(decodeErr, errors.New("invalid Xray status object"))
		} else {
			readString(core["version"], &s.XrayVersion)
			readString(core["state"], &s.XrayState)
			switch s.XrayState {
			case "", "running", "stop", "error":
			default:
				s.XrayState = ""
				decodeErr = errors.Join(decodeErr, errors.New("unknown Xray state"))
			}
		}
	}
	return s, decodeErr
}

func panelStatusMetrics(s *PanelStatus) samples {
	var result samples
	if s == nil {
		return result
	}
	if s.PanelVersion != "" {
		gauge(&result, "xui_panel_info", "Panel version reported by the panel status API.", 1, []string{"version"}, s.PanelVersion)
	}
	if s.XrayVersion != "" {
		gauge(&result, "xui_xray_info", "Xray version reported by the panel status API; not runtime binary attestation.", 1, []string{"version"}, s.XrayVersion)
	}
	if s.XrayState != "" {
		for _, state := range []string{"running", "stop", "error"} {
			gauge(&result, "xui_xray_state", "Xray process state reported by the panel; not an internet health probe.", number(s.XrayState == state), []string{"state"}, state)
		}
	}
	if s.TCPCount != nil {
		gauge(&result, "xui_open_sockets", "Current open network sockets reported by the panel environment.", float64(*s.TCPCount), []string{"protocol"}, "tcp")
	}
	if s.UDPCount != nil {
		gauge(&result, "xui_open_sockets", "Current open network sockets reported by the panel environment.", float64(*s.UDPCount), []string{"protocol"}, "udp")
	}
	return result
}
