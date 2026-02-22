#!/bin/bash
# Full-stack netem interop test: Go UDX server in Docker with network simulation,
# then Dart echo client + full netem test suite.
#
# Usage:
#   ./docker-netem-fullstack.sh [delay_ms] [reorder_pct] [loss_pct]
#   ./docker-netem-fullstack.sh 50 25 2     # 50ms delay, 25% reorder, 2% loss
#   ./docker-netem-fullstack.sh 0 0 0        # Clean network (baseline)
#   ./docker-netem-fullstack.sh              # defaults: 50ms delay, 25% reorder, 2% loss

set -e

DELAY_MS="${1:-50}"
REORDER_PCT="${2:-25}"
LOSS_PCT="${3:-2}"
CONTAINER_NAME="udx-netem-fullstack"
IMAGE_NAME="udx-interop-go-peer"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
AGENTIC_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "=== Full-Stack Netem Interop Test ==="
echo "Network: ${DELAY_MS}ms delay, ${REORDER_PCT}% reorder, ${LOSS_PCT}% loss"
echo ""

# Clean up any previous container
docker rm -f "$CONTAINER_NAME" 2>/dev/null || true

# Build the Docker image using the agentic directory as context
echo "Building Docker image..."
docker build \
    --no-cache \
    -f "$SCRIPT_DIR/Dockerfile" \
    -t "$IMAGE_NAME" \
    "$AGENTIC_DIR"

# Run the container in server mode (supports echo + identify)
echo "Starting Go UDX server in Docker..."
docker run -d \
    --name "$CONTAINER_NAME" \
    --cap-add=NET_ADMIN \
    -p 55223:55223/udp \
    "$IMAGE_NAME" \
    --mode=server --transport=udx --port=55223

# Wait for the server to start
sleep 2

# Get container IP
CONTAINER_IP=$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$CONTAINER_NAME")
echo "Container IP: $CONTAINER_IP"

# Apply network simulation inside the container (only if non-zero)
if [ "$DELAY_MS" -gt 0 ] || [ "$REORDER_PCT" -gt 0 ] || [ "$LOSS_PCT" -gt 0 ]; then
    echo "Applying netem: ${DELAY_MS}ms delay, ${REORDER_PCT}% reorder, ${LOSS_PCT}% loss..."
    NETEM_ARGS="delay ${DELAY_MS}ms ${DELAY_MS}ms"
    if [ "$REORDER_PCT" -gt 0 ]; then
        NETEM_ARGS="$NETEM_ARGS reorder $((100 - REORDER_PCT))% ${REORDER_PCT}%"
    fi
    if [ "$LOSS_PCT" -gt 0 ]; then
        NETEM_ARGS="$NETEM_ARGS loss ${LOSS_PCT}%"
    fi
    docker exec "$CONTAINER_NAME" tc qdisc add dev eth0 root netem $NETEM_ARGS
else
    echo "Clean network (no netem applied)"
fi

# Show server output
echo ""
echo "=== Server output ==="
docker logs "$CONTAINER_NAME" 2>&1
echo ""

# Extract peer ID
PEER_ID=$(docker logs "$CONTAINER_NAME" 2>&1 | grep "PeerID:" | head -1 | sed 's/.*PeerID: //')
if [ -z "$PEER_ID" ]; then
    echo "ERROR: Could not extract PeerID from server logs"
    docker logs "$CONTAINER_NAME" 2>&1
    docker rm -f "$CONTAINER_NAME" 2>/dev/null || true
    exit 1
fi

TARGET="/ip4/127.0.0.1/udp/55223/udx/p2p/$PEER_ID"
echo "Server PeerID: $PEER_ID"
echo "Target: $TARGET"
echo ""

FINAL_RESULT=0

# ── Phase 1: Echo client (existing) ──
echo "=== Phase 1: Echo Client ==="
cd "$AGENTIC_DIR/dart-libp2p"
if dart run bin/interop_echo_client.dart "$TARGET"; then
    echo "Phase 1: PASS"
else
    echo "Phase 1: FAIL"
    FINAL_RESULT=1
fi
echo ""

# ── Phase 2: Full-stack netem test ──
echo "=== Phase 2: Full-Stack Netem Test ==="
if dart run bin/interop_netem_test.dart "$TARGET" 5; then
    echo "Phase 2: PASS"
else
    echo "Phase 2: FAIL"
    FINAL_RESULT=1
fi
echo ""

# ── Diagnostic logs ──
echo "=== Diagnostic Logs ([UDX-DIAG]) ==="
docker logs "$CONTAINER_NAME" 2>&1 | grep "\[UDX-DIAG\]" || echo "(no diagnostic logs)"
echo ""

# ── Full server logs ──
echo "=== Full Server Logs (last 50 lines) ==="
docker logs "$CONTAINER_NAME" 2>&1 | tail -50
echo ""

# Cleanup
docker rm -f "$CONTAINER_NAME" 2>/dev/null || true

if [ $FINAL_RESULT -eq 0 ]; then
    echo "=== ALL TESTS PASSED ==="
else
    echo "=== SOME TESTS FAILED ==="
fi

exit $FINAL_RESULT
