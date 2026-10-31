#!/usr/bin/env bash
#
# Runs every scenario behind the findings in README.md and writes one trace per
# run into a timestamped directory.
#
#   ELEVENLABS_API_KEY=... ./run-all.sh [voice_id]
#
# Takes about 15 minutes, most of it waiting for timeouts. The probe spends
# nearly all that time idle, so it generates almost no load and almost no cost.

set -euo pipefail

if [[ -z "${ELEVENLABS_API_KEY:-}" ]]; then
  echo "ELEVENLABS_API_KEY is not set" >&2
  exit 2
fi

PROBE=./elevenlabs-ws-probe
[[ -x "$PROBE" ]] || go build -o "$PROBE" .

# A voice the account is entitled to use. The default is George, one of the
# standard voices available on free accounts. Pass another voice id as the first
# argument if your account prefers one.
VOICE="${1:-JBFqnCBsd6RMkjVDRZzb}"   # George

# Rachel, a Voice Library voice. Free accounts cannot use library voices over
# the API, so this is chosen deliberately to trigger that failure and show how
# differently each endpoint reports it. On a paid plan this voice may be
# allowed, in which case those two runs produce audio instead of the error.
UNAUTHORISED_VOICE="21m00Tcm4TlvDq8ikWAM"   # Rachel

OUT="traces-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"
echo "writing traces to $OUT/ using voice $VOICE"

run() {
  local name=$1; shift
  echo
  echo "── $name"
  "$PROBE" -voice "$VOICE" -out "$OUT/$name.jsonl" "$@"
}

# Finding 1: an idle timeout closes the socket on one endpoint and only ends
# the context on the other.
run 01-idle-single            -scenario idle                          -max 60s
run 02-idle-multi             -scenario idle    -mode multi           -max 5m

# Finding 3: inactivity_timeout above the documented maximum is clamped.
run 03-clamp-300-to-180       -scenario idle    -mode multi -language tr \
                              -inactivity-timeout 300                 -max 6m

# Finding 4: the same unusable voice is reported differently per endpoint.
echo
echo "── 04-unauthorised-voice-multi (Rachel, a library voice, on purpose)"
"$PROBE" -voice "$UNAUTHORISED_VOICE" -scenario speak -mode multi -max 30s \
         -out "$OUT/04-unauthorised-voice-multi.jsonl"
echo
echo "── 06-unauthorised-voice-single (Rachel, a library voice, on purpose)"
"$PROBE" -voice "$UNAUTHORISED_VOICE" -scenario speak -max 30s \
         -out "$OUT/06-unauthorised-voice-single.jsonl"

# Control: the same request with a permitted voice.
run 05-speak-valid-voice-multi -scenario speak   -mode multi           -max 30s

# Finding 2: a finalised context accepts new text, and a second context opens
# on the same socket.
run 07-revive-context         -scenario revive  -mode multi           -max 90s
run 08-second-context         -scenario newctx  -mode multi           -max 90s

# Behaviour that matches the documentation.
run 09-keepalive-multi        -scenario keepalive -mode multi -keepalive 15s -max 90s
run 10-empty-text-single      -scenario empty                         -max 60s
run 11-empty-text-multi       -scenario empty   -mode multi           -max 60s

echo
echo "done — $(find "$OUT" -name '*.jsonl' | wc -l | tr -d ' ') traces in $OUT/"
