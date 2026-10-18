package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// PanelStatus is the panel's snapshot, not a direct core health check.
// Do not retain errorMsg, host information or other unrelated status fields.
type PanelStatus struct {
	PanelVersion string
	XrayVersion  string
	XrayState    string
}

func (p *Panel) Status(ctx context.Context) (*PanelStatus, error) {
	raw, err := p.object(ctx, http.MethodGet, "/panel/api/server/status")
	if err != nil {
		return nil, err
	}
	return decodePanelStatus(raw)
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
	readString(fields["panelVersion"], &s.PanelVersion)
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
	return result
}
