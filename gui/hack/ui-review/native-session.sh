#!/usr/bin/env bash
# Run only inside a disposable Linux desktop test environment, under dbus-run-session.
set -euo pipefail
: "${UX_APP:?absolute path to rebuilt Wails binary}"
: "${UX_OUT:?absolute test evidence directory}"
: "${UX_STATE:?isolated cache/config/data directory}"
export DISPLAY="${DISPLAY:-:79}"
export PATH="$(dirname "$UX_APP"):$PATH"
export XDG_CACHE_HOME="$UX_STATE/cache" XDG_CONFIG_HOME="$UX_STATE/config" XDG_DATA_HOME="$UX_STATE/data"
export WEBKIT_DISABLE_COMPOSITING_MODE=1 WEBKIT_DISABLE_DMABUF_RENDERER=1 LIBGL_ALWAYS_SOFTWARE=1 GDK_BACKEND=x11
export GTK_MODULES=gail:atk-bridge GNOME_ACCESSIBILITY=1
mkdir -p "$UX_OUT" "$XDG_CACHE_HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME"
if xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; then
  echo "Display $DISPLAY already exists; choose a fresh test display." >&2
  exit 1
fi
Xvfb "$DISPLAY" -screen 0 "${UX_SCREEN:-1600x1000x24}" -nolisten tcp >"$UX_OUT/xvfb.log" 2>&1 &
display_pid=$!
trap 'kill "$display_pid" 2>/dev/null || true' EXIT
sleep 1
kill -0 "$display_pid"
openbox --sm-disable >"$UX_OUT/openbox.log" 2>&1 &
wm_pid=$!
trap 'kill "$wm_pid" "$display_pid" 2>/dev/null || true' EXIT
printf 'export DISPLAY=%q\nexport DBUS_SESSION_BUS_ADDRESS=%q\n' "$DISPLAY" "$DBUS_SESSION_BUS_ADDRESS" >"$UX_OUT/session.env"
"$UX_APP" >"$UX_OUT/app.log" 2>&1 &
app_pid=$!
printf '%s\n' "$app_pid" >"$UX_OUT/app.pid"
wait "$app_pid"
