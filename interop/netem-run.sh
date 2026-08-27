#!/bin/bash
# Runs the go-udx netem baseline across a matrix of network conditions.
# Executes inside the container built by Dockerfile.netem; needs NET_ADMIN.
#
# netem is applied to lo, which both peers use. A packet crosses lo once per
# direction, so "delay Xms" produces an RTT of roughly 2X.
set -u

PAYLOAD="${UDX_NETEM_BYTES:-4194304}"
TIMEOUT_S="${UDX_NETEM_TIMEOUT_S:-120}"

# label:delay_ms:loss_pct:reorder_pct
MATRIX=(
  "clean:0:0:0"
  "delay-25ms:25:0:0"
  "delay-50ms:50:0:0"
  "loss-1pct:25:1:0"
  "loss-3pct:25:3:0"
  "reorder-20pct:25:0:20"
  "mobile-50ms-2pct-25reorder:50:2:25"
)

clear_netem() { tc qdisc del dev lo root 2>/dev/null; }

apply_netem() {
  local delay=$1 loss=$2 reorder=$3
  clear_netem
  [ "$delay" = "0" ] && [ "$loss" = "0" ] && [ "$reorder" = "0" ] && return 0

  local args="netem"
  # Reordering needs a delay to reorder against. Note netem's reorder PERCENT is
  # the fraction of packets sent IMMEDIATELY (i.e. the fraction reordered ahead
  # of the delayed ones) — not the fraction delayed. Passing 100-N here, as
  # docker-netem-fullstack.sh does, silently inverts the condition and produces a
  # near-undelayed link.
  if [ "$reorder" != "0" ]; then
    local d=$delay
    [ "$d" = "0" ] && d=1
    args="$args delay ${d}ms reorder ${reorder}% 50%"
  elif [ "$delay" != "0" ]; then
    args="$args delay ${delay}ms"
  fi
  [ "$loss" != "0" ] && args="$args loss ${loss}%"

  tc qdisc add dev lo root $args
}

echo "=== go-udx netem baseline ==="
echo "payload=$PAYLOAD bytes, per-run timeout=${TIMEOUT_S}s"
echo "note: netem delay is one-way on lo, so RTT is about twice the stated delay"
echo ""

RC=0
for entry in "${MATRIX[@]}"; do
  IFS=":" read -r label delay loss reorder <<< "$entry"
  echo "--- $label (delay=${delay}ms loss=${loss}% reorder=${reorder}%) ---"

  if ! apply_netem "$delay" "$loss" "$reorder"; then
    echo "NETEM_RESULT label=\"$label\" status=SETUP_FAIL"
    RC=1
    continue
  fi

  UDX_NETEM_BASELINE=1 \
  UDX_NETEM_LABEL="$label" \
  UDX_NETEM_BYTES="$PAYLOAD" \
  UDX_NETEM_TIMEOUT_S="$TIMEOUT_S" \
    /udx.test -test.run TestNetemBaseline -test.v -test.timeout "$((TIMEOUT_S + 60))s" 2>&1 \
    | grep -E "NETEM_RESULT|FAIL|panic" || true

  # A failing run should not abort the matrix; the table is the deliverable.
  echo ""
done

clear_netem
exit $RC
