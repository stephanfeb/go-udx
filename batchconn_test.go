package udx

import (
	"net"
	"testing"
	"time"
)

// countDatagrams reads from pc until no datagram arrives for 300ms.
func countDatagrams(t *testing.T, pc net.PacketConn) int {
	t.Helper()
	buf := make([]byte, 2048)
	n := 0
	for {
		pc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, _, err := pc.ReadFrom(buf); err != nil {
			return n
		}
		n++
	}
}

// TestWriteAll_FromDualStackSocketToIPv4 is the socket udx.Dial binds: an
// unspecified "udp" address, which is an IPv6 socket accepting IPv4 too.
// Batched writes to an IPv4 peer used to go through an ipv4.PacketConn
// wrapped around that IPv6 socket, which failed, and the error was ignored:
// every multi-datagram write was silently lost. Only single datagrams (the
// SYN, retransmissions) got through, so a dialer limped along on loss
// recovery at a few dozen KB/s.
func TestWriteAll_FromDualStackSocketToIPv4(t *testing.T) {
	sender, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	io := newDatagramIO(sender)
	bufs := make([][]byte, 8)
	for i := range bufs {
		bufs[i] = []byte{byte(i), 1, 2, 3}
	}
	if err := io.writeAll(bufs, receiver.LocalAddr()); err != nil {
		t.Fatalf("writeAll: %v", err)
	}
	if got := countDatagrams(t, receiver); got != len(bufs) {
		t.Fatalf("receiver got %d of %d datagrams", got, len(bufs))
	}
}

// TestWriteAll_FromIPv4Socket keeps the batched path working where it did.
func TestWriteAll_FromIPv4Socket(t *testing.T) {
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	io := newDatagramIO(sender)
	bufs := make([][]byte, 8)
	for i := range bufs {
		bufs[i] = []byte{byte(i)}
	}
	if err := io.writeAll(bufs, receiver.LocalAddr()); err != nil {
		t.Fatalf("writeAll: %v", err)
	}
	if got := countDatagrams(t, receiver); got != len(bufs) {
		t.Fatalf("receiver got %d of %d datagrams", got, len(bufs))
	}
}
