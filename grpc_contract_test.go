package main

import (
	"context"
	hc "github.com/xtls/xray-core/app/proxyman/command"
	rc "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"testing"
)

type contractHandlers struct {
	hc.HandlerServiceClient
	response *hc.ListOutboundsResponse
}

func (c contractHandlers) ListOutbounds(context.Context, *hc.ListOutboundsRequest, ...grpc.CallOption) (*hc.ListOutboundsResponse, error) {
	return c.response, nil
}

type contractRouting struct {
	rc.RoutingServiceClient
	response *rc.GetBalancerInfoResponse
}

func (c contractRouting) GetBalancerInfo(context.Context, *rc.GetBalancerInfoRequest, ...grpc.CallOption) (*rc.GetBalancerInfoResponse, error) {
	return c.response, nil
}

func TestGRPCResponseContracts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *hc.ListOutboundsResponse
		bad      bool
	}{
		{"nil", nil, true},
		{"empty", &hc.ListOutboundsResponse{}, false},
		{"nil entry", &hc.ListOutboundsResponse{Outbounds: []*core.OutboundHandlerConfig{nil}}, true},
		{"empty tag", &hc.ListOutboundsResponse{Outbounds: []*core.OutboundHandlerConfig{{}}}, true},
		{"duplicate tag", &hc.ListOutboundsResponse{Outbounds: []*core.OutboundHandlerConfig{{Tag: "A"}, {Tag: "A"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &GRPCCore{handlers: contractHandlers{response: tc.response}}
			if _, err := c.Outbounds(context.Background()); (err != nil) != tc.bad {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		response *rc.GetBalancerInfoResponse
		bad      bool
	}{
		{"nil", nil, true},
		{"missing balancer", &rc.GetBalancerInfoResponse{}, true},
		{"empty selection", &rc.GetBalancerInfoResponse{Balancer: &rc.BalancerMsg{}}, false},
		{"empty target tag", &rc.GetBalancerInfoResponse{Balancer: &rc.BalancerMsg{PrincipleTarget: &rc.PrincipleTargetInfo{Tag: []string{""}}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &GRPCCore{routing: contractRouting{response: tc.response}}
			if _, err := c.Balancer(context.Background(), "lb"); (err != nil) != tc.bad {
				t.Fatal(err)
			}
		})
	}
}
