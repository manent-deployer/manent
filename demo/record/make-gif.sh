#!/bin/sh
# Regenerates the README demo GIF (docs/content/assets/demo.gif): a fresh demo container, the
# terminal session (deploy in waves, failed release rolled back, logs, inventory), then the same
# deploys in the web interface.
#
#   docker build -f demo/Dockerfile -t manent-demo .
#   demo/record/make-gif.sh
#
# Requires Docker, python3, agg (https://github.com/asciinema/agg), ffmpeg, Node with
# web/node_modules installed and Google Chrome (used by Playwright, no browser download).
set -eu

cd "$(dirname "$0")/../.."
IMAGE=${IMAGE:-manent-demo}
PORT=${PORT:-18080}
NAME=manent-demo-rec
OUT=${OUT:-docs/content/assets/demo.gif}
WORK=$(mktemp -d)
# KEEP=1 leaves the intermediate files (cast, videos) in $WORK.
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true; [ -n "${KEEP:-}" ] && echo "files in $WORK" || rm -rf "$WORK"' EXIT

# Frame of the GIF: the terminal is padded to the size of the scaled browser, on the
# terminal's background (github-dark theme of agg).
W=960 H=600 BG=0x171b21 FPS=12

echo "starting the demo container and waiting for its history..."
docker rm -f "$NAME" >/dev/null 2>&1 || true
# The identity provider keeps port 9000: it is the issuer the server and the browser both use.
docker run -d --rm -p "$PORT:8080" -p 9000:9000 --name "$NAME" "$IMAGE" >/dev/null
until docker logs "$NAME" 2>&1 | grep -q "demo history ready"; do sleep 2; done

echo "recording the terminal..."
python3 demo/record/terminal.py "$NAME" "$WORK/terminal.cast"
agg --theme github-dark --font-size 15 --idle-time-limit 3.5 --speed 1.25 --fps-cap 15 -q \
  "$WORK/terminal.cast" "$WORK/terminal.gif"

echo "recording the web interface..."
video=$(node demo/record/browser.cjs "http://localhost:$PORT" "$WORK")

# Both parts as video at the same frame rate and size, then a crossfade from the terminal to
# the browser (whose first second is the blank page before the first load).
ffmpeg -v error -y -i "$WORK/terminal.gif" \
  -vf "fps=$FPS,pad=$W:$H:(ow-iw)/2:(oh-ih)/2:color=$BG,format=yuv420p,setsar=1" -c:v libx264 -crf 16 "$WORK/terminal.mp4"
ffmpeg -v error -y -ss 1 -i "$video" \
  -vf "fps=$FPS,scale=$W:$H:flags=lanczos,format=yuv420p,setsar=1" -c:v libx264 -crf 16 "$WORK/browser.mp4"
term=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$WORK/terminal.mp4")
offset=$(echo "$term - 0.6" | bc)
ffmpeg -v error -y -i "$WORK/terminal.mp4" -i "$WORK/browser.mp4" \
  -filter_complex "[0:v][1:v]xfade=transition=fade:duration=0.6:offset=$offset[v]" \
  -map "[v]" -c:v libx264 -crf 16 "$WORK/demo.mp4"

# One palette for the whole GIF; only the changed rectangle is dithered frame by frame.
ffmpeg -v error -y -i "$WORK/demo.mp4" -filter_complex "
  [0:v]split[a][b];[a]palettegen=max_colors=128:stats_mode=diff[p];
  [b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" "$OUT"
ls -l "$OUT"
