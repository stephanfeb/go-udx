/// Bulk-transfer Dart peer for go-udx interop tests.
///
/// Modes:
///   send      <port> <bytes>            connect to a Go server and upload <bytes>
///   recv      <port> <bytes>            connect to a Go server and download <bytes>
///   recvslow  <port> <ms>               download while deliberately reading slowly,
///                                       for <ms> milliseconds, then report consumed
///   sendmulti <port> <bytes> <streams>  upload <bytes> on each of <streams>
///                                       concurrent streams over one connection
///   recvmulti <port> <bytes> <streams>  download <bytes> on each of <streams>
///                                       concurrent streams over one connection
///
/// Writes progress and a final "RESULT <bytes>" line to stderr so the Go side
/// can assert on how much actually crossed. Exits non-zero on failure.
///
/// The payload is the same deterministic pattern the Go tests use
/// (byte i == (i*31 + i~/251) & 0xff) so either side can verify integrity.
///
/// The multi-stream modes exist because both implementations reassemble at the
/// connection: sequence numbers are allocated per connection, so a stream sees
/// only a sparse subsequence of them. Single-stream transfers cannot tell a
/// correct reassembler from one that ignores ordering entirely, and cannot
/// catch bytes delivered intact to the wrong stream. Every stream therefore
/// carries a marker so the receiver verifies identity, not just volume.
import 'dart:async';
import 'dart:io';
import 'dart:typed_data';

import 'package:dart_udx/dart_udx.dart';

Uint8List pattern(int n, int offset) {
  final b = Uint8List(n);
  for (var i = 0; i < n; i++) {
    final k = offset + i;
    b[i] = (k * 31 + k ~/ 251) & 0xff;
  }
  return b;
}

/// markedPattern stamps `marker` over every 512th byte so a receiver can tell
/// which stream a payload belongs to. Byte 0 always carries it, which lets the
/// receiver identify a stream from its very first chunk.
Uint8List markedPattern(int n, int offset, int marker) {
  final b = pattern(n, offset);
  for (var i = 0; i < n; i++) {
    if ((offset + i) % 512 == 0) b[i] = marker;
  }
  return b;
}

Future<void> main(List<String> args) async {
  if (args.length < 3) {
    stderr.writeln('usage: bulk_peer.dart <send|recv> <port> <bytes>');
    exit(2);
  }
  final mode = args[0];
  final port = int.parse(args[1]);
  final totalBytes = int.parse(args[2]);
  final streamCount = args.length > 3 ? int.parse(args[3]) : 1;

  final rawSocket = await RawDatagramSocket.bind(InternetAddress.anyIPv4, 0);
  final mux = UDXMultiplexer(rawSocket);
  final udpSocket = mux.createSocket(UDX(), '127.0.0.1', port);

  final stream = await UDXStream.createOutgoing(
    UDX(), udpSocket, 1, 0, '127.0.0.1', port,
  );
  await udpSocket.handshakeComplete.timeout(const Duration(seconds: 10));
  stderr.writeln('READY');

  // Everything opened here, so cleanup closes all of it rather than leaking
  // the extra streams the multi-stream modes create.
  final allStreams = <UDXStream>[stream];
  var exitCode0 = 0;
  try {
    if (mode == 'send') {
      const chunk = 32 * 1024;
      var sent = 0;
      final started = DateTime.now();
      while (sent < totalBytes) {
        final n = (totalBytes - sent) < chunk ? (totalBytes - sent) : chunk;
        await stream.add(pattern(n, sent));
        sent += n;
        if (sent % (1024 * 1024) == 0) {
          stderr.writeln('PROGRESS $sent');
        }
      }
      final ms = DateTime.now().difference(started).inMilliseconds;
      stderr.writeln('RESULT $sent');
      stderr.writeln('ELAPSED_MS $ms');
    } else if (mode == 'recv') {
      var got = 0;
      var corrupt = false;
      final done = Completer<void>();
      stream.data.listen((chunk) {
        final expect = pattern(chunk.length, got);
        for (var i = 0; i < chunk.length; i++) {
          if (chunk[i] != expect[i]) {
            corrupt = true;
            break;
          }
        }
        got += chunk.length;
        if (got % (1024 * 1024) < chunk.length) {
          stderr.writeln('PROGRESS $got');
        }
        if (got >= totalBytes && !done.isCompleted) done.complete();
      }, onDone: () {
        if (!done.isCompleted) done.complete();
      });
      await done.future.timeout(const Duration(seconds: 120));
      stderr.writeln('RESULT $got');
      if (corrupt) {
        stderr.writeln('CORRUPT');
        exitCode0 = 1;
      }
    } else if (mode == 'recvslow') {
      // Read deliberately slowly. Under consumption-anchored flow control the
      // sender must stall rather than push its whole payload into our buffer.
      // totalBytes is reinterpreted as a duration in milliseconds here.
      var got = 0;
      late StreamSubscription<Uint8List> sub;
      sub = stream.data.listen((chunk) {
        got += chunk.length;
        // Pausing propagates back through the mapped stream to the controller,
        // so nothing further is counted as consumed while we are asleep.
        sub.pause(Future.delayed(const Duration(milliseconds: 25)));
      });
      await Future.delayed(Duration(milliseconds: totalBytes));
      await sub.cancel();
      stderr.writeln('RESULT $got');
      stderr.writeln('WINDOW ${stream.receiveWindow}');
    } else if (mode == 'sendmulti' || mode == 'recvmulti') {
      // All streams share one connection, and therefore one sequence space.
      // The first is already open; the rest take the remaining odd local IDs.
      final streams = <UDXStream>[stream];
      for (var i = 1; i < streamCount; i++) {
        streams.add(await UDXStream.createOutgoing(
          UDX(), udpSocket, 1 + 2 * i, 0, '127.0.0.1', port,
        ));
      }
      allStreams.addAll(streams.skip(1));

      var total = 0;
      var corrupt = false;

      if (mode == 'sendmulti') {
        await Future.wait(List.generate(streams.length, (i) async {
          const chunk = 32 * 1024;
          var n = 0;
          while (n < totalBytes) {
            final len = (totalBytes - n) < chunk ? (totalBytes - n) : chunk;
            await streams[i].add(markedPattern(len, n, i));
            n += len;
          }
          total += n;
          stderr.writeln('STREAM_DONE $i $n');
        }));
      } else {
        // The receiver does not know which stream carries which payload until
        // the first byte arrives, which is the point: identity is proven by
        // the data, not assumed from the order streams were opened.
        final seen = <int>{};
        await Future.wait(streams.map((s) async {
          var n = 0;
          int? marker;
          final done = Completer<void>();
          s.data.listen((chunk) {
            marker ??= chunk[0];
            final expect = markedPattern(chunk.length, n, marker!);
            for (var i = 0; i < chunk.length; i++) {
              if (chunk[i] != expect[i]) {
                corrupt = true;
                break;
              }
            }
            n += chunk.length;
            if (n >= totalBytes && !done.isCompleted) done.complete();
          }, onDone: () {
            if (!done.isCompleted) done.complete();
          });
          await done.future.timeout(const Duration(seconds: 120));
          total += n;
          if (marker != null && !seen.add(marker!)) {
            corrupt = true;
            stderr.writeln('DUPLICATE_MARKER $marker');
          }
          stderr.writeln('STREAM_DONE $marker $n');
        }));
        if (seen.length != streamCount) {
          corrupt = true;
          stderr.writeln('MARKERS ${seen.length} want $streamCount');
        }
      }

      stderr.writeln('RESULT $total');
      if (corrupt) {
        stderr.writeln('CORRUPT');
        exitCode0 = 1;
      }
    } else {
      stderr.writeln('unknown mode: $mode');
      exitCode0 = 2;
    }
  } catch (e, st) {
    stderr.writeln('ERROR $e');
    stderr.writeln(st.toString());
    exitCode0 = 1;
  }

  for (final s in allStreams) {
    try {
      await s.close();
    } catch (_) {}
  }
  mux.close();
  exit(exitCode0);
}
