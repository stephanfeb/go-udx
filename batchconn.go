package udx

import (
	"net"

	"golang.org/x/net/ipv4"
)

// datagramIO is how the multiplexer talks to its socket. On Linux, and when
// the socket is a real *net.UDPConn, it moves several datagrams per syscall
// (recvmmsg and sendmmsg through x/net); anywhere else it falls back to one
// datagram per call. The syscall was the cost: after the ACK policy change a
// server moving 137 KB pages still made one sendto per 1,372-byte chunk and
// one recvfrom per arriving datagram, all from one goroutine.
//
// A wrapped PacketConn (tests count datagrams that way) takes the fallback,
// so a count taken through a wrapper is exact, and the batch path is only
// ever measured through the kernel's own counters.
type datagramIO struct {
	pc    net.PacketConn
	batch *ipv4.PacketConn // nil: no batching on this socket
}

// readBatchSize is how many datagrams one read call may return, and how many
// buffers the read loop keeps for it.
const readBatchSize = 64

func newDatagramIO(pc net.PacketConn) *datagramIO {
	d := &datagramIO{pc: pc}
	if uc, ok := pc.(*net.UDPConn); ok {
		d.batch = ipv4.NewPacketConn(uc)
	}
	return d
}

// readMessages fills ms from the socket and returns how many it filled. Each
// message must carry one buffer; N and Addr are set on return.
func (d *datagramIO) readMessages(ms []ipv4.Message) (int, error) {
	if d.batch != nil {
		return d.batch.ReadBatch(ms, 0)
	}
	n, addr, err := d.pc.ReadFrom(ms[0].Buffers[0])
	if err != nil {
		return 0, err
	}
	ms[0].N = n
	ms[0].Addr = addr
	return 1, nil
}

// writeAll sends every buffer to addr, several per syscall where it can. A
// short batch write is retried for the remainder; the kernel may accept
// fewer than offered.
func (d *datagramIO) writeAll(bufs [][]byte, addr net.Addr) error {
	if d.batch == nil || len(bufs) == 1 {
		for _, b := range bufs {
			if _, err := d.pc.WriteTo(b, addr); err != nil {
				return err
			}
		}
		return nil
	}
	ms := make([]ipv4.Message, len(bufs))
	for i, b := range bufs {
		ms[i].Buffers = [][]byte{b}
		ms[i].Addr = addr
	}
	for len(ms) > 0 {
		n, err := d.batch.WriteBatch(ms, 0)
		if err != nil {
			return err
		}
		if n <= 0 {
			n = 1
		}
		ms = ms[n:]
	}
	return nil
}
