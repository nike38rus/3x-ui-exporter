package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Explicit opt-in: read-only calls against a running core, never reset counters.
func TestLiveCore(t *testing.T) {
	address := os.Getenv("XUI_TEST_GRPC")
	if address == "" {
		t.Skip("XUI_TEST_GRPC not set")
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := newCore(conn)
	tests := []struct {
		name string
		call func(context.Context) error
	}{
		{"outbounds", func(ctx context.Context) error {
			v, e := c.Outbounds(ctx)
			if e == nil {
				t.Logf("runtime outbounds: %d", len(v))
			}
			return e
		}},
		{"balancer", func(ctx context.Context) error {
			tag := os.Getenv("XUI_TEST_BALANCER")
			if tag == "" {
				tag = "someTest"
			}
			v, e := c.Balancer(ctx, tag)
			if e == nil && v == nil {
				return fmt.Errorf("balancer %s absent in runtime", tag)
			}
			if e == nil && v != nil {
				t.Logf("selected targets: %d", len(v.Targets))
			}
			return e
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := test.call(ctx); err != nil {
				t.Error(err)
			}
		})
	}
}
