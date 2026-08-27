#!/bin/bash
# Builds and runs the go-udx congestion-control baseline under simulated network
# conditions, then prints a summary table.
#
# Usage:
#   ./netem-baseline.sh [payload_bytes] [per_run_timeout_s]
#   ./netem-baseline.sh 4194304 120     # defaults
#
# Why this exists: CUBIC, pacing and RTT sampling were unreachable until d9b1dc7
# — ACKs never reached the congestion controller — so every performance figure
# this project produced before then described a different system, and all were
# taken on clean loopback. This measures the live controller under delay, loss
# and reordering.
set -u

PAYLOAD="${1:-4194304}"
TIMEOUT_S="${2:-120}"
IMAGE="go-udx-netem"
CONTAINER="go-udx-netem-run"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

echo "Building $IMAGE..."
if ! docker build -q -f "$SCRIPT_DIR/Dockerfile.netem" -t "$IMAGE" "$REPO_DIR" >/dev/null; then
    echo "docker build failed" >&2
    exit 1
fi

OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

docker run --rm --name "$CONTAINER" \
    --cap-add=NET_ADMIN \
    -e UDX_NETEM_BYTES="$PAYLOAD" \
    -e UDX_NETEM_TIMEOUT_S="$TIMEOUT_S" \
    "$IMAGE" 2>&1 | tee "$OUT"

echo ""
echo "=== Summary ==="
printf "%-28s %-6s %8s %9s %10s %8s %9s %8s\n" \
    CONDITION STATUS MB/s ELAPSED RETX_OVHD CWND SRTT_MS MINRTT
printf "%-28s %-6s %8s %9s %10s %8s %9s %8s\n" \
    ---------- ------ ---- ------- --------- ---- ------- ------

grep "^NETEM_RESULT" "$OUT" | while read -r line; do
    get() { sed -n "s/.*[[:space:]]$1=\([^[:space:]]*\).*/\1/p" <<< "$line"; }
    label=$(sed -n 's/.*label="\([^"]*\)".*/\1/p' <<< "$line")
    printf "%-28s %-6s %8s %8sms %9s%% %8s %9s %7sms\n" \
        "$label" "$(get status)" "$(get mbps)" "$(get elapsed_ms)" \
        "$(get retransmit_overhead_pct)" "$(get cwnd)" "$(get srtt_ms)" "$(get minrtt_ms)"
done

echo ""
if grep -q "status=FAIL\|status=SETUP_FAIL" "$OUT"; then
    echo "At least one condition FAILED — see the run log above."
    exit 1
fi
echo "All conditions completed."
