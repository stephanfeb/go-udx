# Recommendations from go-ricochet

**Context:** we reported the ~256KB per-connection stall, you fixed it in `d9b1dc7`,
and we verified the fix against our original reproductions. This is what we think
is worth doing next, ordered by risk.

Verification on our side, for the record:

| Reproduction | Before | After |
|---|---|---|
| `GET` 32KB in a loop | stalled at read 7, 229,376 cumulative bytes | 90 reads, 2.8MB, 132ms, contents intact |
| `PUT` 205KB in a loop | stalled on the 2nd write | 15 writes, 2.9MB, 191ms |
| 500-document vault sync | could not complete | 10 requests, ~300ms |

---

## P0 — Verify Go↔Dart WINDOW_UPDATE interop before this reaches mobile clients

This is the only item we consider a live risk.

The fix changed what the WINDOW_UPDATE value *means* on the wire: it is now an
absolute offset (`dataConsumed + recvWindow`) that grows for the life of the
stream. `dart-udx` has not been updated and still implements size semantics.

Specifically, `dart-udx/lib/src/stream.dart`:

```dart
void deliverWindowUpdate(int windowSize) {
  _remoteReceiveWindow = windowSize;
  ...
  if (inflight < cwnd && inflight < _remoteReceiveWindow && connWindowAvailable > 0) {
```

Dart's sender gates **`inflight`** — bytes currently outstanding — against the
advertised value. Against an offset that grows without bound, that comparison
stops binding almost immediately, which would leave `cwnd` as the only limit on
a Dart→Go upload and make stream-level flow control ineffective in that
direction.

The reverse direction may work by coincidence: Dart's receiver advertises
`_receiveWindow`, grown by cumulative bytes received (`stream.dart:226`), which
numerically resembles an offset, so a Go sender comparing lifetime `dataSent`
against it lands in roughly the right place. Working by coincidence is not the
same as working.

**We have not proven this** — it is inferred from reading, not from a failing
test. But it is cheap to settle:

- Sustained Dart→Go upload with a **deliberately slow reader** on the Go side.
  Assert that the sender actually blocks and that Go's `recvBuf` stays bounded.
  If flow control is ineffective, the buffer grows without limit.
- The same in reverse.

**The deeper point:** two implementations currently disagree about what a wire
field means, and nothing written down settles it. Whatever the test shows, the
semantics belong in a short spec note next to the frame definition — this class
of divergence recurs otherwise.

## P1 — Make the interop suite able to catch this class of bug

The suite is three tests (`TestGoDartInterop`, `TestDartMuxToGoMux`,
`TestGoMuxToDartMux`) with payloads of 2,000 and 4,096 bytes. Nothing in it ever
reaches a window update, which is why a 256KB ceiling survived in a project that
already has a Docker + netem harness.

Any test that moved 300KB would have caught the original bug on day one.

Suggested additions, all using `docker-netem-fullstack.sh`:

- Multi-megabyte sustained transfer, both directions.
- A slow-consumer case (receiver deliberately not reading) — the flow-control path.
- The above under induced loss and delay, not just on loopback.

## P2 — Add a guard for the "silently unreachable" failure mode

The congestion-controller bug is the one worth generalising from. Nothing
errored, no test failed, and the defect was a `!= nil` guard that read as
ordinary defensive coding while swallowing 100% of calls:

```go
acked := c.pm.HandleAckFrame(f)            // deletes each entry
for _, seq := range acked {
    if pkt := c.pm.GetPacket(seq); pkt != nil {   // always nil
        c.cc.OnPacketAcked(...)
    }
}
```

Unit tests on CUBIC pass regardless, because they call the controller directly.
Only the wiring was broken, and nothing was asserting on the wiring.

Two concrete suggestions:

- **Assert observable controller state in an e2e test**, not just in unit tests:
  `cwnd` has moved off its initial value, an RTT sample was taken, `inflight`
  returns to zero once a transfer completes. Any of those would have failed.
- **Grep for the same shape elsewhere** — a nil-guard on a lookup that follows a
  delete. Where a nil result is *expected* rather than *impossible*, an
  unexpected nil should log or panic in tests rather than continue silently.

## P3 — Re-baseline under netem, and treat CUBIC as new code

Every performance number this project has ever produced was measured with the
congestion controller disconnected — the 8MB/2.17s and 64MB at 54MB/s figures
included, if those were taken on loopback. That is not a criticism of the
numbers; it is that they describe a different system than the one that now
ships.

CUBIC, pacing and RTT sampling have effectively never executed in production.
They are newly live code paths. Loss and delay are where their behaviour
actually matters, and where a fresh baseline is worth having before anyone tunes
against it.

## P4 — Schedule the two wire changes together

Two known items both need a coordinated Dart change:

1. **Per-stream offset in `StreamFrame`**, for the concurrent-streams bug.
2. **The uint32 window field**, which caps a stream's lifetime transfer at 4GB.

Doing them as one wire revision is cheaper than two rounds of coordination.

On the 4GB cap specifically: saturating rather than wrapping is the right
choice, but it is worth confirming what a stream *does* on arrival. If it stalls
permanently, that is the 256KB bug again with a bigger number — and 4GB is
reachable for us. Our roadmap has long-lived connections moving content-addressed
blobs in bulk; a mobile client syncing a large vault over weeks on one connection
gets there.

## P5 — Connection-level flow control

Agreed with leaving it. Enforcing a 1MB cap with no `MAX_DATA` sender would only
relocate the cliff, and go-ricochet runs a single UDX stream per connection, so
per-stream limits are the ones that bind for us. Worth a comment pointing at the
decision so the next reader doesn't take the absence for an oversight.

---

## What we can offer as a downstream canary

`go-ricochet`'s `TestVaultSyncBatched` pushes ~1MB of documents across one
connection and asserts the sync completes in a bounded number of requests. It
fails loudly against the pre-`d9b1dc7` transport. If it is useful to you as an
integration-level smoke test against a real application workload, take it.

Also worth knowing: we verified that
`go-libp2p-udx-transport/transport.go:122` opens exactly one UDX stream per
connection, so the concurrent-streams bug genuinely does not reach go-ricochet.
That is load-bearing for your decision to defer it — if the transport ever
changes to multiplex at the UDX layer instead of relying on yamux, the priority
of P4.1 changes with it.
