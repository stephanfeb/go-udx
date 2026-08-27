/// Bulk-transfer Dart peer for go-udx interop tests.
///
/// Modes:
///   send  <port> <bytes>          connect to a Go server and upload <bytes>
///   recv  <port> <bytes>          connect to a Go server and download <bytes>
///
/// Writes progress and a final "RESULT <bytes>" line to stderr so the Go side
/// can assert on how much actually crossed. Exits non-zero on failure.
///
/// The payload is the same deterministic pattern the Go tests use
/// (byte i == (i*31 + i~/251) & 0xff) so either side can verify integrity.
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

Future<void> main(List<String> args) async {
  if (args.length < 3) {
    stderr.writeln('usage: bulk_peer.dart <send|recv> <port> <bytes>');
    exit(2);
  }
  final mode = args[0];
  final port = int.parse(args[1]);
  final totalBytes = int.parse(args[2]);

  final rawSocket = await RawDatagramSocket.bind(InternetAddress.anyIPv4, 0);
  final mux = UDXMultiplexer(rawSocket);
  final udpSocket = mux.createSocket(UDX(), '127.0.0.1', port);

  final stream = await UDXStream.createOutgoing(
    UDX(), udpSocket, 1, 0, '127.0.0.1', port,
  );
  await udpSocket.handshakeComplete.timeout(const Duration(seconds: 10));
  stderr.writeln('READY');

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
    } else {
      stderr.writeln('unknown mode: $mode');
      exitCode0 = 2;
    }
  } catch (e, st) {
    stderr.writeln('ERROR $e');
    stderr.writeln(st.toString());
    exitCode0 = 1;
  }

  try {
    await stream.close();
  } catch (_) {}
  mux.close();
  exit(exitCode0);
}
