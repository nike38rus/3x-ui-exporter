package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
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
	Onlines(context.Context) (int, error)
}

type InboundStatus struct {
	Tag    string
	Enable bool
}

type InboundSource interface {
	Inbounds(context.Context) ([]*InboundStatus, error)
}

func (p *Panel) Status(ctx context.Context) (*PanelStatus, error) {
	raw, err := p.object(ctx, http.MethodGet, "/panel/api/server/status")
	if err != nil {
		return nil, err
	}
	return decodePanelStatus(raw)
}

func (p *Panel) Onlines(ctx context.Context) (int, error) {
	raw, err := p.object(ctx, http.MethodPost, "/panel/api/clients/onlines")
	if err != nil {
		return 0, err
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

func decodeInbounds(raw json.RawMessage) ([]*InboundStatus, error) {
	type rawInbound struct {
		Tag    *string `json:"tag"`
		Enable *bool   `json:"enable"`
	}
	var list []rawInbound
	if json.Unmarshal(raw, &list) != nil {
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
		seen[tag] = true
		result = append(result, &InboundStatus{
			Tag:    tag,
			Enable: *item.Enable,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Tag < result[j].Tag
	})
	return result, nil
}

func inboundStatusMetrics(inbounds []*InboundStatus) samples {
	var result samples
	for _, in := range inbounds {
		gauge(&result, "xui_inbound_enabled", "Whether inbound is administratively enabled in the panel.", number(in.Enable), []string{"inbound"}, in.Tag)
	}
	return result
}

func decodeOnlines(raw json.RawMessage) (int, error) {
	var onlines []string
	if json.Unmarshal(raw, &onlines) != nil {
		return 0, errors.New("invalid onlines array")
	}
	return len(onlines), nil
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
