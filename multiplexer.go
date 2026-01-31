package udx

import (
	"context"
	"fmt"
	"net"
	"sync"
)

// Multiplexer binds a UDP socket and routes packets by destination CID.
type Multiplexer struct {
	mu   sync.Mutex
	conn net.PacketConn
	clk  Clock

	// CID -> Connection routing
	connections map[string]*Connection // key: hex(CID)

	// Incoming connection acceptance
	incoming chan *Connection

	// Lifecycle
	closeOnce sync.Once
	closeCh   chan struct{}
}

// NewMultiplexer creates a new multiplexer on the given PacketConn.
func NewMultiplexer(conn net.PacketConn, clk Clock) *Multiplexer {
	m := &Multiplexer{
		conn:        conn,
		clk:         clk,
		connections: make(map[string]*Connection),
		incoming:    make(chan *Connection, 16),
		closeCh:     make(chan struct{}),
	}
	go m.readLoop()
	return m
}

// Accept waits for an incoming connection.
func (m *Multiplexer) Accept(ctx context.Context) (*Connection, error) {
	select {
	case c := <-m.incoming:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.closeCh:
		return nil, ErrConnectionClosed
	}
}

// Dial creates an outgoing connection to the given address.
func (m *Multiplexer) Dial(ctx context.Context, addr net.Addr) (*Connection, error) {
	localCID, err := RandomConnectionID(DefaultCIDLength)
	if err != nil {
		return nil, fmt.Errorf("generating local CID: %w", err)
	}
	remoteCID, err := RandomConnectionID(DefaultCIDLength)
	if err != nil {
		return nil, fmt.Errorf("generating remote CID: %w", err)
	}

	c := NewConnection(localCID, remoteCID, m.conn.LocalAddr(), addr, true, m.clk,
		func(data []byte, addr net.Addr) error {
			_, err := m.conn.WriteTo(data, addr)
			return err
		})

	m.mu.Lock()
	m.connections[localCID.String()] = c
	m.mu.Unlock()

	// Send a SYN packet to initiate the connection
	c.mu.Lock()
	c.state = ConnStateEstablished
	c.addrValidated = true // initiator knows the address
	c.mu.Unlock()

	// Send SYN frame so the remote multiplexer creates the connection
	c.sendFrames([]Frame{&StreamFrame{IsSyn: true}})

	return c, nil
}

// Close shuts down the multiplexer and all connections.
func (m *Multiplexer) Close() error {
	m.closeOnce.Do(func() {
		close(m.closeCh)

		m.mu.Lock()
		for _, c := range m.connections {
			c.Close()
		}
		m.connections = make(map[string]*Connection)
		m.mu.Unlock()

		m.conn.Close()
	})
	return nil
}

// Addr returns the local address.
func (m *Multiplexer) Addr() net.Addr {
	return m.conn.LocalAddr()
}

// ConnectionCount returns the number of active connections.
func (m *Multiplexer) ConnectionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.connections)
}

func (m *Multiplexer) readLoop() {
	buf := make([]byte, MaxMTU+100)
	for {
		select {
		case <-m.closeCh:
			return
		default:
		}

		n, addr, err := m.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-m.closeCh:
				return
			default:
				continue
			}
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		m.handleDatagram(data, addr)
	}
}

func (m *Multiplexer) handleDatagram(data []byte, addr net.Addr) {
	pkt, err := UnmarshalPacket(data)
	if err != nil {
		// Try version negotiation
		if len(data) >= 4 {
			// Could send version negotiation response
		}
		return
	}

	// Route by destination CID
	cidKey := pkt.DestinationCID.String()

	m.mu.Lock()
	c, ok := m.connections[cidKey]
	m.mu.Unlock()

	if ok {
		c.HandlePacket(pkt)
		return
	}

	// Unknown CID — possibly a new connection
	// Check if it has a SYN frame
	hasSyn := false
	for _, f := range pkt.Frames {
		if sf, ok := f.(*StreamFrame); ok && sf.IsSyn {
			hasSyn = true
			break
		}
	}

	if !hasSyn {
		return // Drop unknown non-SYN packet
	}

	// Create new connection for incoming
	localCID := pkt.DestinationCID
	remoteCID := pkt.SourceCID

	newConn := NewConnection(localCID, remoteCID, m.conn.LocalAddr(), addr, false, m.clk,
		func(data []byte, addr net.Addr) error {
			_, err := m.conn.WriteTo(data, addr)
			return err
		})
	newConn.mu.Lock()
	newConn.state = ConnStateEstablished
	newConn.addrValidated = false // Anti-amplification until validated
	newConn.mu.Unlock()

	m.mu.Lock()
	m.connections[cidKey] = newConn
	m.mu.Unlock()

	// Deliver the initial packet
	newConn.HandlePacket(pkt)

	// Notify acceptor
	select {
	case m.incoming <- newConn:
	default:
	}
}
