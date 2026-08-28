# Known gaps

Things this transport does not currently do, why they are deliberate or
unfinished, and what closing each would involve. Written down so the next reader
does not have to rediscover them, and so that "not done" is distinguishable from
"not noticed".

Current as of 2026-08-28.

---

## 1. Retransmission can still abandon data

**Status:** open. The most substantial correctness gap remaining.

`PacketManager.scheduleRetransmission` gives up after `MaxRetransmitRetries`
attempts. Because retransmission re-sends a packet under its *original*
sequence number, the retry budget belongs to that packet, and exhausting it
abandons the bytes it carried. The stream then has a permanent hole and cannot
complete.

`Connection.abandonStream` makes that visible rather than silent: the stream is
reset, so the application gets `ErrStreamReset` instead of a hang. That is the
honest outcome for an unrecoverable stream, but it is a way of reporting the
gap, not of closing it.

**This is not what QUIC does.** RFC 9002 has no retry limit. Lost data is
re-framed into a *new* packet with a *new* packet number — packet numbers are
never reused (RFC 9000 §12.3) — so the retry budget never attaches to the data.
Delivery is bounded by the connection's idle timeout, not by a count, and
individual streams are never reset because of loss.

Adding per-stream byte offsets was expected to close this and does not. The hole
moved from a sequence number to a byte offset; it is still a hole. What actually
closes it is retransmitting under fresh sequence numbers, which needs:

- `Connection.retransmitPacket` to allocate a new sequence rather than reuse the
  old one, and the packet manager to transfer tracking from the old number to
  the new;
- the retry budget to move from the packet to the connection, so exhausting it
  ends the connection (as an idle timeout would) rather than killing one stream;
- care that the congestion controller sees exactly one `OnPacketSent` per
  transmission and does not double-count, which is a mistake this code has
  already made once (see `d9b1dc7`).

The current behaviour is safe, just weaker than the RFC's: a stream that hits it
fails cleanly instead of recovering.

---

## 2. `MaxIdleTimeout` is declared but never enforced

**Status:** open, and it interacts with the above.

`MaxIdleTimeout` exists in `constants.go` and nothing reads it. A connection with
no traffic is never closed, which is why exhausting the retransmission budget is
currently the *only* thing that surfaces an unrecoverable packet to the
application.

QUIC's rules (RFC 9000 §10.1) are worth copying if this is implemented:

- negotiated as a transport parameter, with the effective value the **minimum**
  of what the two ends advertise (zero disables);
- restarted on receiving and successfully processing a packet, and on sending an
  ack-eliciting packet when none has been sent since the last receipt;
- **at least three times the current PTO**, so loss recovery always gets a
  chance before the connection is declared idle. This is dynamic — a fixed
  constant is wrong on a slow path, where the retransmission budget stretches
  with the RTO and can exceed a fixed 30s;
- expiry closes the connection **silently**, with no CONNECTION_CLOSE.

Deferred because it is a design question — when should a quiet connection die? —
rather than a defect with one obvious answer.

---

## 3. Connection-level flow control is accounted but not enforced

**Status:** deliberate. Do not "fix" without also writing the sender side.

Documented at the top of `flow_control.go`. `MAX_DATA` is tracked, but nothing
blocks on the connection-level limit. Enforcing it without also implementing a
`MAX_DATA` sender would relocate the stall that per-stream flow control already
fixed, from 256KB to 1MB, rather than removing it.

Per-stream limits are the ones that bind in every current deployment.

---

## 4. Reordering is expensive, and the reason is structural

**Status:** understood, measured, not worth chasing with the obvious fix.

Loss detection follows RFC 9002 §6.1: a packet is declared lost once it is
`LossReorderThreshold` (3) behind the largest acknowledged packet, or older than
9/8 of the larger of the smoothed and latest RTT.

Those thresholds were added expecting a throughput win and did not deliver one.
`kPacketThreshold` tolerates three packets of displacement; on a fast link even a
sub-millisecond delay displaces far more than three, so the threshold is
exceeded and the packet is declared lost anyway. Measured against
`loss_detection_test.go`, the retransmission rate moved by less than a point at
every displacement from 200us to 5ms and every reordering rate from 2% to 20%.

The thresholds are kept because declaring every gap lost on sight is wrong in
principle, not because they pay.

Two cautions for anyone revisiting this:

- **The netem percentages are not a measurement.** The same condition on
  unchanged code has produced 27.9%, 28.9%, 33.1%, 36.8% and 40.5% retransmit
  overhead. Use `loss_detection_test.go`, which is seeded and deterministic.
  netem remains valuable for *hangs*, which are unambiguous — it caught two that
  the unit tests missed.
- **Simulating a network is harder than it looks.** Two plausible harnesses gave
  confident, entirely fictional numbers before the third was right: a goroutine
  per datagram lets the scheduler shuffle delivery, and a per-lane sleep
  serialises the link. Both reported heavy loss on a clean path. The invariant
  worth holding onto is `TestLossDetection_CleanPathNeverRetransmits`: an
  ordered, lossless link must provoke no retransmission at all.

---

## 5. Multi-stream support differs between the two stacks

**Status:** informational, but easy to get wrong.

The Go and Dart stacks multiplex at different layers:

| | Go | Dart |
|---|---|---|
| libp2p muxer | yamux, over a single UDX stream | **UDX itself** (`streamMultiplexer: '/udx/1.0.0'`) |
| UDX streams per connection | exactly one | one per libp2p stream |

Concurrent UDX streams are a production path for dart-libp2p and unused by
go-libp2p-udx-transport. Anything that changes stream multiplexing must be
tested against the Dart side; the Go stack alone will not exercise it.
`interop/multistream_interop_test.go` covers four concurrent streams in each
direction against the real Dart implementation.
