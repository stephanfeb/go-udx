# go-udx

A Go implementation of the UDX protocol — a QUIC-inspired reliable UDP transport with built-in stream multiplexing, congestion control, and flow control.

## Features

- **Stream multiplexing** — Multiple independent bidirectional streams over a single UDP connection
- **CUBIC congestion control** — RFC 9002-based RTT estimation with CUBIC congestion avoidance
- **Packet pacing** — Smooth send rate to avoid bursts
- **Flow control** — Connection-level and per-stream flow control with automatic window updates
- **Path MTU Discovery** — RFC 8899 binary search between 1280–1500 bytes
- **Connection migration** — PATH_CHALLENGE/PATH_RESPONSE for address validation
- **Anti-amplification** — 3x amplification limit for unvalidated addresses
- **Variable-length Connection IDs** — CID-based packet routing for multiplexing

## Installation

```
go get github.com/stephanfeb/go-udx
```

## Quick Start

### Server

```go
package main

import (
    "context"
    "fmt"
    "io"

    "github.com/stephanfeb/go-udx"
)

func main() {
    mux, err := udx.Listen("0.0.0.0:9000")
    if err != nil {
        panic(err)
    }
    defer mux.Close()

    conn, err := mux.Accept(context.Background())
    if err != nil {
        panic(err)
    }

    stream, err := conn.AcceptStream(context.Background())
    if err != nil {
        panic(err)
    }

    buf := make([]byte, 1024)
    n, _ := stream.Read(buf)
    fmt.Printf("received: %s\n", buf[:n])

    stream.Write(buf[:n]) // echo back
    stream.Close()
}
```

### Client

```go
package main

import (
    "context"
    "fmt"

    "github.com/stephanfeb/go-udx"
)

func main() {
    conn, err := udx.Dial(context.Background(), "127.0.0.1:9000")
    if err != nil {
        panic(err)
    }

    stream, err := conn.OpenStream(context.Background())
    if err != nil {
        panic(err)
    }

    stream.Write([]byte("hello from go-udx"))
    stream.CloseWrite()

    buf := make([]byte, 1024)
    n, _ := stream.Read(buf)
    fmt.Printf("echo: %s\n", buf[:n])
}
```

## Wire Format (v2)

```
┌──────────┬────────────┬────────┬────────────┬────────┬─────────┬──────────────┬──────────────┬────────┐
│ Version  │ DstCIDLen  │ DstCID │ SrcCIDLen  │ SrcCID │ SeqNum  │ DstStreamID  │ SrcStreamID  │ Frames │
│ 4 bytes  │ 1 byte     │ var    │ 1 byte     │ var    │ 4 bytes │ 4 bytes      │ 4 bytes      │ var    │
└──────────┴────────────┴────────┴────────────┴────────┴─────────┴──────────────┴──────────────┴────────┘
```

## Architecture

```
udx.go              Top-level API (Listen, Dial)
multiplexer.go      UDP socket binding, CID-based packet routing
connection.go       Peer connection, stream registry, frame dispatch
stream.go           Reliable ordered stream (io.ReadWriteCloser)
congestion.go       CUBIC congestion controller, RTT estimation
pacing.go           Packet pacing
packet_manager.go   Sent packet tracking, ACK/SACK processing, retransmission
flow_control.go     Connection-level and per-stream flow control
pmtud.go            Path MTU Discovery (RFC 8899)
packet.go           Wire format marshal/unmarshal
frame.go            Frame type definitions (STREAM, ACK, RESET, etc.)
cid.go              Connection ID generation and encoding
constants.go        Protocol parameters and tuning constants
clock.go            Clock abstraction (real + mock for testing)
errors.go           Error definitions
version.go          Protocol version negotiation
```

## Frame Types

| Frame | Description |
|-------|-------------|
| STREAM | Data transfer with SYN/FIN flags |
| ACK | Acknowledgment with SACK ranges |
| WINDOW_UPDATE | Per-stream flow control |
| MAX_DATA | Connection-level flow control |
| RESET_STREAM | Abrupt stream termination |
| STOP_SENDING | Request sender to stop |
| CONNECTION_CLOSE | Graceful connection shutdown |
| PING | Keepalive / RTT measurement |
| PATH_CHALLENGE / PATH_RESPONSE | Address validation |
| MAX_STREAMS | Stream limit advertisement |
| NEW_CONNECTION_ID / RETIRE_CONNECTION_ID | CID lifecycle |
| MTU_PROBE | PMTUD probing |
| DATA_BLOCKED / STREAM_DATA_BLOCKED | Flow control signaling |
| PADDING | Packet padding |

## Interoperability

go-udx is wire-compatible with [dart-udx](../dart-udx). Cross-language interop tests verify packet round-trips in both directions.

```bash
go test -v ./interop/
```

## Testing

```bash
go test -race ./...
```

## License

MIT
