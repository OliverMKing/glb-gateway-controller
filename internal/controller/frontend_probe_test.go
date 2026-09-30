package controller

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestTCPRegionalFrontendProber(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	prober := &TCPRegionalFrontendProber{Dialer: net.Dialer{Timeout: time.Second}}
	if err := prober.Probe(context.Background(), "127.0.0.1", []int32{int32(port)}); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if err := prober.Probe(context.Background(), "", []int32{int32(port)}); err == nil {
		t.Fatal("Probe() should reject an empty address")
	}
}
