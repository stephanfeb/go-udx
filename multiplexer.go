package udx

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// earlyPacket holds a packet that arrived before its connection's SYN.
type earlyPacket struct {
	pkt  *Packet
	addr net.Addr
}

// Multiplexer binds a UDP socket and routes packets by destination CID.
type Multiplexer struct {
	mu   sync.Mutex
	conn net.PacketConn
	clk  Clock

	// CID -> Connection routing
	connections map[string]*Connection // key: hex(CID)

	// Buffer for packets that arrive before the connection SYN.
	// Keyed by destination CID string. Capped to prevent memory abuse.
	earlyPackets map[string][]earlyPacket

	// Incoming connection acceptance
	incoming chan *Connection

	// Lifecycle
	closeOnce sync.Once
	closeCh   chan struct{}
}

// NewMultiplexer creates a new multiplexer on the given PacketConn.
func NewMultiplexer(conn net.PacketConn, clk Clock) *Multiplexer {
	m := &Multiplexer{
		conn:         conn,
		clk:          clk,
		connections:  make(map[string]*Connection),
		earlyPackets: make(map[string][]earlyPacket),
		incoming:     make(chan *Connection, 16),
		closeCh:      make(chan struct{}),
	}
	go m.readLoop()
	go m.idleTimeoutLoop()
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

	cidKey := localCID.String()
	c.onClose = func() { m.removeConnection(cidKey) }

	m.mu.Lock()
	m.connections[cidKey] = c
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

		// Collect connections and clear the map before closing them,
		// so onClose callbacks don't deadlock trying to acquire m.mu.
		m.mu.Lock()
		conns := make([]*Connection, 0, len(m.connections))
		for _, c := range m.connections {
			conns = append(conns, c)
		}
		m.connections = make(map[string]*Connection)
		m.mu.Unlock()

		for _, c := range conns {
			c.Close()
		}

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

// removeConnection removes a connection from the routing map.
func (m *Multiplexer) removeConnection(cidKey string) {
	m.mu.Lock()
	delete(m.connections, cidKey)
	m.mu.Unlock()
}

// idleTimeoutLoop periodically checks for closed connections and removes them.
func (m *Multiplexer) idleTimeoutLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			m.mu.Lock()
			for cidKey, c := range m.connections {
				select {
				case <-c.closeCh:
					delete(m.connections, cidKey)
				default:
				}
			}
			m.mu.Unlock()
		}
	}
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
		// Buffer the packet — it may have arrived before the SYN due to
		// UDP reordering over the internet. Cap at 32 packets per CID
		// and 256 total CIDs to prevent memory abuse.
		m.mu.Lock()
		if len(m.earlyPackets) < 256 {
			buf := m.earlyPackets[cidKey]
			if len(buf) < 32 {
				m.earlyPackets[cidKey] = append(buf, earlyPacket{pkt: pkt, addr: addr})
			}
		}
		m.mu.Unlock()
		return
	}

	// Create new connection for incoming
	localCID := pkt.DestinationCID
	remoteCID := pkt.SourceCID

	newConn := NewConnection(localCID, remoteCID, m.conn.LocalAddr(), addr, false, m.clk,
		func(data []byte, addr net.Addr) error {
			_, err := m.conn.WriteTo(data, addr)
			return err
		})
	newConn.onClose = func() { m.removeConnection(cidKey) }
	newConn.mu.Lock()
	newConn.state = ConnStateEstablished
	newConn.addrValidated = true // Validated by receiving a valid SYN
	newConn.mu.Unlock()

	m.mu.Lock()
	m.connections[cidKey] = newConn
	// Grab any early-arriving packets for this CID
	early := m.earlyPackets[cidKey]
	delete(m.earlyPackets, cidKey)
	m.mu.Unlock()

	// Deliver the SYN packet first
	newConn.HandlePacket(pkt)

	// Replay early packets in arrival order
	for _, ep := range early {
		newConn.HandlePacket(ep.pkt)
	}

	// Notify acceptor
	select {
	case m.incoming <- newConn:
	default:
	}
}
