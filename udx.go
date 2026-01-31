package udx

import (
	"context"
	"fmt"
	"net"
)

// Option configures a Multiplexer or Connection.
type Option func(*options)

type options struct {
	clock Clock
}

// WithClock sets a custom clock (for testing).
func WithClock(c Clock) Option {
	return func(o *options) { o.clock = c }
}

func applyOptions(opts []Option) *options {
	o := &options{clock: RealClock{}}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Listen creates a Multiplexer bound to the given address.
func Listen(addr string, opts ...Option) (*Multiplexer, error) {
	o := applyOptions(opts)

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolving address: %w", err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listening: %w", err)
	}

	return NewMultiplexer(conn, o.clock), nil
}

// Dial creates a connection to the given address.
func Dial(ctx context.Context, addr string, opts ...Option) (*Connection, error) {
	o := applyOptions(opts)

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolving address: %w", err)
	}

	// Bind to any local port
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, fmt.Errorf("binding: %w", err)
	}

	mux := NewMultiplexer(conn, o.clock)
	return mux.Dial(ctx, udpAddr)
}

// MetricsObserver is an optional interface for observing UDX metrics.
type MetricsObserver interface {
	OnCongestionWindowUpdate(cid ConnectionID, oldCwnd, newCwnd int, reason string)
	OnRTTSample(cid ConnectionID, latest, smoothed, rttVar interface{})
	OnPacketLoss(cid ConnectionID, seq uint32, lossType string)
}
