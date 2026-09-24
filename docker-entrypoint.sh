#!/bin/bash
# Entrypoint monitoring-api: start Xvfb headful sendiri lalu exec binary.
# Alasan: xvfb-run (`wait`-based) berperilaku tidak konsisten sebagai PID 1
# di beberapa kondisi; integrasi manual ini deterministik.
set -o pipefail


XAUTH_FILE="/tmp/.Xauthority"
touch "$XAUTH_FILE"
MCOOKIE="$(mcookie)"

# Pilih display bebas: mulai dari 99, cek lock file lama & penggunaan.
find_display() {
    local n=99
    while [ -e "/tmp/.X${n}-lock" ] || xdpyinfo -display ":$n" >/dev/null 2>&1; do
        n=$((n + 1))
        [ "$n" -gt 200 ] && { echo "FATAL: tidak ada display bebas" >&2; exit 1; }
    done
    echo "$n"
}
XVFB_SCREEN="${XVFB_SCREEN:-1280x1024x24}"
XVFB_DISPLAY=":$(find_display 2>/dev/null || echo 99)"

echo "add $XVFB_DISPLAY MIT-MAGIC-COOKIE-1 $MCOOKIE" \
    | XAUTHORITY="$XAUTH_FILE" xauth source - >/dev/null 2>&1

Xvfb "$XVFB_DISPLAY" -screen 0 "$XVFB_SCREEN" -nolisten tcp \
    -auth "$XAUTH_FILE" &
XVFB_PID=$!

# Tunggu X server siap (maks 15 detik). xdpyinfo wajib bawa XAUTHORITY.
xcheck() { XAUTHORITY="$XAUTH_FILE" xdpyinfo -display "$XVFB_DISPLAY" >/dev/null 2>&1; }
for _ in $(seq 1 30); do
    if xcheck; then
        break
    fi
    sleep 0.5
done
if ! xcheck; then
    echo "FATAL: Xvfb gagal ready di display $XVFB_DISPLAY" >&2
    kill "$XVFB_PID" 2>/dev/null || true
    exit 1
fi
echo "entrypoint: Xvfb $XVFB_DISPLAY ready (pid $XVFB_PID)"

export DISPLAY="$XVFB_DISPLAY"
export XAUTHORITY="$XAUTH_FILE"

trap 'kill "$XVFB_PID" 2>/dev/null || true; exit 0' TERM INT
# CMD dari param (default monitoring-api); collector CLI butuh sekali jalan
# tanpa restart (ENTRYPOINT_ONCE=1).
CMD_BIN="${1:-}"
[ -n "${CMD_BIN:-}" ] || CMD_BIN=/usr/local/bin/monitoring-api
if [ $# -gt 0 ]; then shift; fi
while true; do
    "$CMD_BIN" "$@" &
    APP_PID=$!
    wait "$APP_PID" || true
    if [ "${ENTRYPOINT_ONCE:-0}" = "1" ]; then
        echo "entrypoint: CMD selesai, mode once, keluar" >&2
        exit 0
    fi
    echo "entrypoint: monitoring-api exited, restart 5s" >&2
    sleep 5
done
