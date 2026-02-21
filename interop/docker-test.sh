#!/bin/bash
# Builds and runs the Go UDX peer in Docker with simulated network conditions.
# Then runs the Dart interop tests against it.
#
# Usage:
#   ./docker-test.sh [delay_ms] [reorder_pct]
#   ./docker-test.sh 50 25    # 50ms delay, 25% packet reordering
#   ./docker-test.sh           # defaults: 30ms delay, 20% reorder

set -e

DELAY_MS="${1:-30}"
REORDER_PCT="${2:-20}"
CONTAINER_NAME="udx-interop-test"
IMAGE_NAME="udx-interop-go-peer"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
AGENTIC_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "=== UDX Docker Interop Test ==="
echo "Network simulation: ${DELAY_MS}ms delay, ${REORDER_PCT}% reorder"
echo ""

# Clean up any previous container
docker rm -f "$CONTAINER_NAME" 2>/dev/null || true

# Build the Docker image using the agentic directory as context
echo "Building Docker image..."
docker build \
    -f "$SCRIPT_DIR/Dockerfile" \
    -t "$IMAGE_NAME" \
    "$AGENTIC_DIR"

# Run the container with NET_ADMIN capability (needed for tc)
echo "Starting Go UDX peer in Docker..."
docker run -d \
    --name "$CONTAINER_NAME" \
    --cap-add=NET_ADMIN \
    -p 55223:55223/udp \
    "$IMAGE_NAME"

# Wait for the server to start
sleep 2

# Get container IP for direct access
CONTAINER_IP=$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$CONTAINER_NAME")
echo "Container IP: $CONTAINER_IP"

# Apply network simulation inside the container
echo "Applying network conditions: ${DELAY_MS}ms delay, ${REORDER_PCT}% reorder..."
docker exec "$CONTAINER_NAME" tc qdisc add dev eth0 root netem \
    delay "${DELAY_MS}ms" "${DELAY_MS}ms" \
    reorder "$((100 - REORDER_PCT))%" "${REORDER_PCT}%"

# Show the server's output
echo ""
echo "=== Server output ==="
docker logs "$CONTAINER_NAME"
echo ""

# Extract peer ID and address from server output
PEER_ID=$(docker logs "$CONTAINER_NAME" 2>&1 | grep "PeerID:" | sed 's/.*PeerID: //')
echo "Server PeerID: $PEER_ID"
echo "Server address: /ip4/127.0.0.1/udp/55223/udx/p2p/$PEER_ID"
echo ""

# Run the Dart interop echo client against the Docker container
echo "=== Running Dart echo client ==="
cd "$AGENTIC_DIR/dart-libp2p"
dart run bin/interop_echo_client.dart "/ip4/127.0.0.1/udp/55223/udx/p2p/$PEER_ID"
RESULT=$?

echo ""
if [ $RESULT -eq 0 ]; then
    echo "=== TEST PASSED ==="
else
    echo "=== TEST FAILED ==="
    echo ""
    echo "Server logs:"
    docker logs "$CONTAINER_NAME" 2>&1 | tail -50
fi

# Cleanup
docker rm -f "$CONTAINER_NAME" 2>/dev/null || true

exit $RESULT
