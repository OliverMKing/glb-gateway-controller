package controller

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"
)

type RegionalFrontendProber interface {
	Probe(ctx context.Context, address string, ports []int32) error
}

// TCPRegionalFrontendProber keeps unusable regional listeners out of the GLB pool.
type TCPRegionalFrontendProber struct {
	Dialer net.Dialer
}

func NewTCPRegionalFrontendProber() *TCPRegionalFrontendProber {
	return &TCPRegionalFrontendProber{Dialer: net.Dialer{Timeout: 3 * time.Second}}
}

func (p *TCPRegionalFrontendProber) Probe(ctx context.Context, address string, ports []int32) error {
	if address == "" {
		return fmt.Errorf("regional frontend has no public IP address")
	}
	// Every global listener must be reachable through the regional frontend.
	for _, port := range ports {
		connection, err := p.Dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(int(port))))
		if err != nil {
			return fmt.Errorf("connect to %s port %d: %w", address, port, err)
		}
		if err := connection.Close(); err != nil {
			return fmt.Errorf("close connection to %s port %d: %w", address, port, err)
		}
	}
	return nil
}
