package udx

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// natProxy stands between a client and a server the way a carrier NAT
// does: the server sees the proxy's outbound port, and rebind moves the
// client to a new outbound port, as a NAT does after its mapping for an idle
// client expires. Replies the server sends to the old port are lost.
type natProxy struct {
	t      *testing.T
	inside *net.UDPConn // the client talks to this
	server *net.UDPAddr

	// maxSize, when set, drops every datagram larger than it, as a link
	// with a small MTU does (a carrier's tunnel, 464XLAT) when the packet
	// may not be fragmented.
	maxSize int

	mu      sync.Mutex
	outside *net.UDPConn
	client  net.Addr
}

// newNATProxy starts a proxy to server; maxSize, when not 0, is the
// largest datagram it passes.
func newNATProxy(t *testing.T, server net.Addr, maxSize int) *natProxy {
	t.Helper()
	inside, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := &natProxy{t: t, inside: inside, server: server.(*net.UDPAddr), maxSize: maxSize}
	p.rebind()
	go p.fromClient()
	t.Cleanup(func() {
		inside.Close()
		p.mu.Lock()
		p.outside.Close()
		p.mu.Unlock()
	})
	return p
}

func (p *natProxy) addr() net.Addr { return p.inside.LocalAddr() }

// rebind gives the client a new outbound port; the old one is gone.
func (p *natProxy) rebind() {
	out, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		p.t.Fatal(err)
	}
	p.mu.Lock()
	old := p.outside
	p.outside = out
	p.mu.Unlock()
	if old != nil {
		old.Close()
	}
	go p.fromServer(out)
}

func (p *natProxy) fromClient() {
	buf := make([]byte, 65536)
	for {
		n, from, err := p.inside.ReadFrom(buf)
		if err != nil {
			return
		}
		if p.maxSize > 0 && n > p.maxSize {
			continue
		}
		p.mu.Lock()
		p.client = from
		out := p.outside
		p.mu.Unlock()
		out.WriteTo(buf[:n], p.server)
	}
}

func (p *natProxy) fromServer(out *net.UDPConn) {
	buf := make([]byte, 65536)
	for {
		n, _, err := out.ReadFrom(buf)
		if err != nil {
			return
		}
		if p.maxSize > 0 && n > p.maxSize {
			continue
		}
		p.mu.Lock()
		client := p.client
		p.mu.Unlock()
		if client != nil {
			p.inside.WriteTo(buf[:n], client)
		}
	}
}

// A client whose NAT moves it to a new port keeps its connection: the
// server answers at the new address once the path is validated.
func TestConnection_SurvivesNATRebinding(t *testing.T) {
	server, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The server echoes every stream.
	go func() {
		sc, err := server.Accept(context.Background())
		if err != nil {
			return
		}
		for {
			s, err := sc.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go io.Copy(s, s)
		}
	}()

	nat := newNATProxy(t, server.Addr(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := client.Dial(ctx, nat.addr())
	if err != nil {
		t.Fatal(err)
	}

	echo := func(msg string) error {
		s, err := conn.OpenStream(ctx)
		if err != nil {
			return err
		}
		defer s.Close()
		if _, err := s.Write([]byte(msg)); err != nil {
			return err
		}
		s.SetReadDeadline(time.Now().Add(3 * time.Second))
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(s, got); err != nil {
			return err
		}
		if string(got) != msg {
			t.Fatalf("echo: got %q, want %q", got, msg)
		}
		return nil
	}

	if err := echo("before"); err != nil {
		t.Fatalf("before the rebinding: %v", err)
	}
	nat.rebind()
	if err := echo("after"); err != nil {
		t.Fatalf("after the rebinding: %v", err)
	}
}

// A path that carries less than a full Ethernet frame (a mobile carrier's
// tunnel) still gets a reply larger than one datagram: what the
// connection sends fits a 1280-byte IPv6 minimum MTU.
func TestConnection_ReplyCrossesSmallMTUPath(t *testing.T) {
	server, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The server answers each stream with 8 KB.
	reply := make([]byte, 8*1024)
	for i := range reply {
		reply[i] = byte(i)
	}
	go func() {
		sc, err := server.Accept(context.Background())
		if err != nil {
			return
		}
		for {
			s, err := sc.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 1)
				if _, err := io.ReadFull(s, buf); err == nil {
					s.Write(reply)
				}
			}()
		}
	}()

	nat := newNATProxy(t, server.Addr(), 1350-28) // a 1350-byte path, less the IPv4 and UDP headers
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := client.Dial(ctx, nat.addr())
	if err != nil {
		t.Fatal(err)
	}
	s, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	s.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(s, got); err != nil {
		t.Fatalf("the reply did not cross the path: %v", err)
	}
}
