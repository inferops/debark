#!/bin/sh
# Run only in a disposable, dedicated Linux accessibility test container.
# Does not launch/restart Debark or change host accessibility settings.
set -eu
UX_SESSION_ROOT=${1:?absolute isolated evidence directory containing session.env}
. "$UX_SESSION_ROOT/session.env"
export DISPLAY DBUS_SESSION_BUS_ADDRESS
export GNOME_ACCESSIBILITY=1 GTK_MODULES=gail:atk-bridge
export WEBKIT_DISABLE_COMPOSITING_MODE=1
UX_ORCA_ROOT="$UX_SESSION_ROOT/orca-alsa"
export UX_ORCA_ROOT
export XDG_CONFIG_HOME="$UX_ORCA_ROOT/config"
export XDG_DATA_HOME="$UX_ORCA_ROOT/data"
export XDG_RUNTIME_DIR="$UX_ORCA_ROOT/runtime"
export ALSA_CONFIG_PATH="$UX_ORCA_ROOT/alsa-null.conf"
mkdir -p "$XDG_CONFIG_HOME/speech-dispatcher" "$XDG_DATA_HOME/orca" "$XDG_RUNTIME_DIR"
chmod 700 "$XDG_RUNTIME_DIR"

python3 - <<'PY'
import json
import os
from pathlib import Path

module = next(Path('/usr/lib').rglob('sd_dummy'), None)
if module is None:
    raise SystemExit('speech-dispatcher dummy module unavailable; no speech evidence recorded')
conf = Path(os.environ['XDG_CONFIG_HOME']) / 'speech-dispatcher' / 'speechd.conf'
conf.write_text('LogLevel 3\nAudioOutputMethod "alsa"\nAudioALSADevice "null"\nAddModule "dummy" "' + str(module) + '" ""\nDefaultModule "dummy"\nDefaultLanguage "en"\n')
# sd_dummy still opens audio to play its fallback error message. Both the
# explicit device and default device are discard-only in this helper process.
Path(os.environ['ALSA_CONFIG_PATH']).write_text('pcm.!default { type null }\npcm.null { type null }\n')
general = {
    'activeProfile': ['Default', 'default'],
    'startingProfile': ['Default', 'default'],
    'enableSpeech': True,
    'enableBraille': False,
    'enableKeyEcho': False,
    'enableEchoByCharacter': False,
    'speechServerFactory': 'orca.speechdispatcherfactory',
    'speechServerInfo': ['Default Synthesizer', 'default'],
}
profile = {'general': dict(general, profile=['Default', 'default']), 'keybindings': {}, 'pronunciations': {}}
settings = {'general': general, 'profiles': {'default': profile}, 'keybindings': {}, 'pronunciations': {}}
(Path(os.environ['XDG_DATA_HOME']) / 'orca' / 'user-settings.conf').write_text(json.dumps(settings))
PY

gdbus call --session --dest org.a11y.Bus --object-path /org/a11y/bus --method org.a11y.Bus.GetAddress
speech-dispatcher --run-daemon --config-dir="$XDG_CONFIG_HOME/speech-dispatcher" > "$UX_ORCA_ROOT/speech-dispatcher.stdout" 2> "$UX_ORCA_ROOT/speech-dispatcher.stderr"
sleep 1
python3 "$(dirname "$0")/native-orca-launcher.py" --replace --debug-file="$UX_ORCA_ROOT/orca.debug" > "$UX_ORCA_ROOT/orca.stderr" 2>&1 &
printf '%s\n' "$!" > "$UX_ORCA_ROOT/orca.pid"
sleep 2
win=$(xdotool search --onlyvisible --name '^Debark$' | tail -n 1)
test -n "$win"
xdotool windowactivate --sync "$win"
printf 'Orca launched; inspect %s/orca.stderr and SPEECH OUTPUT before claiming speech evidence.\n' "$UX_ORCA_ROOT"

# For subsequent actions source session.env again in the calling shell.
# Send native XTest input with xdotool key (without --window).
# Log each action in a separate journal, never append markers to orca.debug:
# before=$(wc -l < /out/S4-final2/orca-alsa/orca.debug)
# xdotool key --clearmodifiers --delay 200 F10 Down Up Escape
# sleep 1
# sed -n "$((before+1)),\$p" /out/S4-final2/orca-alsa/orca.debug | grep 'SPEECH OUTPUT'
