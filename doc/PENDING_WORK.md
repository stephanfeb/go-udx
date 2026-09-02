# Known gaps

Things this transport does not currently do, why they are deliberate or
unfinished, and what closing each would involve. Written down so the next reader
does not have to rediscover them, and so that "not done" is distinguishable from
"not noticed".

Current as of 2026-08-28.

---

## 1. Retransmission can still abandon data

**Status:** CLOSED (2026-08-30). Retransmission now re-sends under fresh sequence
numbers and no longer abandons data; §2 below is the backstop.

`PacketManager.scheduleRetransmission` used to give up after
`MaxRetransmitRetries` attempts. Because a retransmission re-sent a packet under
its *original* sequence number, the retry budget belonged to that packet, and
exhausting it abandoned the bytes it carried — `Connection.abandonStream` reset
the stream (`ErrStreamReset`) rather than let it hang. Safe, but weaker than the
RFC: a recoverable stream failed instead of recovering. Under the sustained loss
of a real relay path this reset store-and-forward streams mid-transfer, which is
what stalled device pairing (the relay read the reset as `EOF`).

**The fix (QUIC's model, RFC 9002 — no retry limit; RFC 9000 §12.3 — packet
numbers are never reused):**

- `PacketManager.Retransmit` allocates a **new** sequence for the packet and
  transfers its tracking (and its retransmit timer) from the old number to the
  new. Safe because both v3 stacks reassemble streams on byte offsets, not
  sequence order: a duplicate is discarded by its offset, and the retransmission
  is acknowledged unambiguously (no Karn ambiguity — `SentTime` is reset on the
  re-key, so the RTT sample is honest).
- The per-packet retry cap is **gone**. A packet is re-sent for as long as the
  connection lives; what ends a genuinely dead path is §2's idle timeout closing
  the whole connection, not a count resetting one stream.
- Congestion accounting stays correct without double-counting: the bytes stay in
  flight across the re-key, so a retransmission calls **no** `OnPacketSent`, and
  ACK-based loss detection contracts the window through the new
  `CongestionController.OnCongestionEvent` (cwnd only, idempotent per recovery
  epoch) instead of the old inflight-decrementing `OnPacketLost`. This also fixed
  a latent gap: SACK-detected loss previously informed congestion control of
  nothing at all.

The RTO timer and SACK-driven loss detection both route through `Retransmit`,
which collapses a resend that the other has already covered within the last RTO,
so one loss produces one resend.

Covered by `retransmit_recovery_test.go` (re-key, SentTime reset, trigger
collapse, acked no-op) and the interop suite against the real Dart stack.

---

## 2. `MaxIdleTimeout` is declared but never enforced

**Status:** CLOSED (2026-08-30). Enforced by a per-connection watchdog.

`MaxIdleTimeout` used to sit in `constants.go` with nothing reading it, so a
connection with no traffic was never closed. That is now the backstop that ends
a dead path (which §1 no longer does by exhausting a retry count):

- `Connection` stamps `lastActivity` on every received packet (`HandlePacket`)
  and a self-rearming watchdog (`armIdleCheck`/`onIdleCheck`) closes the
  connection once it has been silent longer than `idleTimeout()`.
- `idleTimeout()` is `max(MaxIdleTimeout, 3 × PTO)` — **at least three PTOs**
  (RFC 9000 §10.1), so loss recovery always gets a chance first. The RTO is
  capped at 5s today, so the 30s floor wins in practice; the `max()` keeps it
  correct if that cap ever rises.
- Expiry closes the connection **silently** — `closeInternal(..., announce:
  false)` sends no CONNECTION_CLOSE (RFC 9000 §10.1) but still resets the streams
  so blocked readers and writers wake with `ErrStreamReset` rather than hang.

Not negotiated as a transport parameter (the effective-value = min-of-both-ends
rule); this is a local enforcement of the local constant. Wire negotiation is a
larger protocol change and was not needed to close the gap.

Covered by `retransmit_recovery_test.go` (idle-close wakes readers, receipt
keeps a connection alive, the ≥3×PTO relationship) and, end-to-end over real
sockets, `TestWriter_OnADeadPathFailsRatherThanHanging`.

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
