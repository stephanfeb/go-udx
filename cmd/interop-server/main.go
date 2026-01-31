// interop-server is a raw UDX v2 packet echo server for cross-language testing.
// Protocol: receives UDX v2 packets, echoes STREAM frame data back with swapped CIDs/stream IDs.
// Prints "READY <port>" to stderr when listening.
package main

import (
	"fmt"
	"net"
	"os"

	udx "github.com/stephanfeb/go-udx"
)

func main() {
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve: %v\n", err)
		os.Exit(1)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	port := conn.LocalAddr().(*net.UDPAddr).Port
	fmt.Fprintf(os.Stderr, "READY %d\n", port)

	buf := make([]byte, 2000)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read: %v\n", err)
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		pkt, err := udx.UnmarshalPacket(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "unmarshal: %v\n", err)
			continue
		}

		// Find STREAM frames and echo them back
		for _, frame := range pkt.Frames {
			sf, ok := frame.(*udx.StreamFrame)
			if !ok || len(sf.Data) == 0 {
				continue
			}

			// Build response: swap CIDs and stream IDs
			resp := &udx.Packet{
				Version:             udx.VersionV2,
				DestinationCID:      pkt.SourceCID,
				SourceCID:           pkt.DestinationCID,
				Sequence:            pkt.Sequence + 1,
				DestinationStreamID: pkt.SourceStreamID,
				SourceStreamID:      pkt.DestinationStreamID,
				Frames: []udx.Frame{
					&udx.StreamFrame{Data: sf.Data},
				},
			}

			respBytes := udx.MarshalPacket(resp)
			_, err := conn.WriteToUDP(respBytes, remoteAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "write: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "echoed %d bytes to %s\n", len(sf.Data), remoteAddr)
			}
		}
	}
}
