# Patch for dart-udx: honour the advertised stream offset

**Status:** proposed, not applied and **not yet executed** — this is a `dart-udx`
change, written up here because `go-udx`'s interop suite is what demonstrates the
defect. The diffs below are derived from reading `dart-udx@5291cdb`; they have
not been compiled or run. Validate by applying them and re-running the interop
test named below, which fails today and should go green.

**Proves it:** `interop/bulk_interop_test.go` → `TestBulk_DartToGoSlowConsumer`.
That test fails today and goes green when this patch lands.

## The defect

`go-udx` advertises stream flow control as an **absolute offset**: the highest
cumulative byte position the peer may send (`dataConsumed + recvWindow`, QUIC
`MAX_STREAM_DATA` semantics, RFC 9000 §4.1).

`dart-udx`'s sender compares that offset against **bytes currently outstanding**
— `lib/src/stream.dart:170`, `:273`, `:344`:

```dart
if (inflight < cwnd && inflight < _remoteReceiveWindow && connWindowAvailable > 0) {
```

Against an offset that grows for the life of the stream, that comparison stops
binding almost immediately, leaving `cwnd` as the only limit on a Dart → Go
upload. Measured, with a deliberately slow Go reader:

```
Go consumed 243,048 bytes; 2,030,400 buffered unread; advertised window 524,288
→ the Dart sender ran 3.9x past the offset it was granted
```

There is a second bug on the same line: `inflight` comes from the
**connection-level** congestion controller (`stream.dart:123`), so it is the
wrong quantity for a per-stream limit regardless of the semantics question.

## What is *not* wrong

Worth stating plainly, because it narrows the fix considerably: **the wire
format is fine and both implementations already send the same thing.**

`dart-udx`'s *receiver* already advertises an absolute offset —
`stream.dart:228`:

```dart
_bytesReceivedSinceWindowUpdate += data.length;
if (_bytesReceivedSinceWindowUpdate > _initialReceiveWindow ~/ 4) {
  _receiveWindow += _bytesReceivedSinceWindowUpdate;
```

`_receiveWindow` is `65536 + cumulativeBytesReceived`. That is an offset with a
constant 64KB slack; it is only *named* like a window size. So Go comparing its
lifetime `dataSent` against it is correct flow control, not a coincidence.

Only Dart's **sender-side interpretation** is wrong. No wire change is required.

(Also worth noting: the comment at `stream.dart:222` shows dart-udx independently
hit the same divergence bug go-udx did, and worked around it by pinning the
trigger to the initial window rather than the growing one.)

## Patch 1 — gate on cumulative bytes sent

`stream.dart:93` already maintains exactly the counter needed:

```dart
int bytesWritten = 0;          // :93
bytesWritten += fragment.length;   // :387, in the send path
```

Replace the sender's gate at **`:170`**, **`:273`** and **`:344`**:

```diff
-if (inflight < cwnd && inflight < _remoteReceiveWindow && connWindowAvailable > 0) {
+if (inflight < cwnd && bytesWritten < _remoteReceiveWindow && connWindowAvailable > 0) {
```

At `:344` the fragment length is in scope, so prefer the exact form there — it
is what stops a write overshooting the granted offset by up to one fragment:

```diff
-if (inflight < cwnd && inflight < _remoteReceiveWindow && connWindowAvailable > 0) {
+if (inflight < cwnd &&
+    bytesWritten + fragment.length <= _remoteReceiveWindow &&
+    connWindowAvailable > 0) {
```

`inflight < cwnd` stays: that is congestion control, a separate and correct
concern. Only the flow-control half of the condition changes.

## Patch 2 — survive the 4GB wrap

`WindowUpdateFrame.windowSize` is a uint32 (`packet.dart:286-306`), so an offset
is transmitted **modulo 2³²**. `go-udx` now reconstructs the full value against
the limit it already holds rather than clamping, because clamping stalls a
stream permanently at 4GB — the original 256KB ceiling with a bigger number on
it. `dart-udx` needs the same on both sides.

Sending (`stream.dart:234` and `:419`) — mask explicitly rather than relying on
`ByteData.setUint32` truncation:

```diff
-[WindowUpdateFrame(windowSize: _receiveWindow)],
+[WindowUpdateFrame(windowSize: _receiveWindow & 0xFFFFFFFF)],
```

Receiving (`stream.dart:269`):

```diff
 void deliverWindowUpdate(int windowSize) {
-  _remoteReceiveWindow = windowSize;
+  // The offset travels modulo 2^32. Recover the full value by choosing the
+  // candidate nearest the limit we already hold (RFC 1982 serial arithmetic).
+  // Unambiguous because the true offset is always within one receive window
+  // of the current limit, and the window is orders of magnitude below 2^32.
+  const modulus = 1 << 32;
+  final base = _remoteReceiveWindow & ~(modulus - 1);
+  var best = base + windowSize;
+  for (final cand in [best - modulus, best + modulus]) {
+    if ((cand - _remoteReceiveWindow).abs() < (best - _remoteReceiveWindow).abs()) {
+      best = cand;
+    }
+  }
+  // Monotonic: a stale or reordered frame must never revoke granted credit.
+  if (best > _remoteReceiveWindow) _remoteReceiveWindow = best;
```

The equivalent Go implementation is `reconstructWindowOffset` in
`flow_control.go`, with tests in `flow_control_window_test.go` covering the
boundary exhaustively.

## Verifying

From `go-udx`:

```
go test ./interop/ -run TestBulk -v
```

`TestBulk_DartToGoSlowConsumer` asserts the precise invariant a compliant sender
must satisfy — `BufferedBytes() <= RecvWindow()` — and names this document when
it fails. `TestBulk_DartToGoUpload` and `TestBulk_GoToDartDownload` already pass
and should stay passing.

## Naming, optional but recommended

`_receiveWindow` / `_remoteReceiveWindow` / `WindowUpdateFrame.windowSize` all
hold absolute offsets, not window sizes. The mismatch between the name and the
value is what produced this bug in the first place, on both sides
independently. Renaming to `_maxRecvOffset` / `_remoteMaxSendOffset` /
`maxStreamData` would make the next such divergence much harder to write.
