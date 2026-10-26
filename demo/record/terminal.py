#!/usr/bin/env python3
"""Records the terminal part of the demo GIF as an asciicast v2 file.

Every command runs for real in the demo container (docker exec -t, so the manent client sees
a terminal and prints tables and live progress); only the typing is simulated.

    python3 demo/record/terminal.py <container> <out.cast>
"""
import fcntl
import json
import os
import pty
import select
import struct
import sys
import termios
import time

COLS, ROWS = 100, 26
PROMPT = "\x1b[1;32m$\x1b[0m "

# (command, pause after the output in seconds)
SCENES = [
    ("cat shop-1.3.0.yml", 2.5),
    ("manent deploy -f shop-1.3.0.yml", 2.5),
    ("manent deploy -f shop-1.4.0.yml --id shop-1.4.0", 2.5),
    ("manent logs shop-1.4.0 --level warn", 3.5),
    ("manent inventory", 4.0),
]


def main():
    container, out = sys.argv[1], sys.argv[2]
    events = []
    start = time.monotonic()

    def emit(text):
        events.append([round(time.monotonic() - start, 4), "o", text])

    def clear():
        emit("\x1b[H\x1b[2J")

    time.sleep(0.5)
    for i, (cmd, pause) in enumerate(SCENES):
        # Every scene starts on a clean screen, so the GIF never scrolls mid-sentence.
        if i > 0:
            clear()
        emit(PROMPT)
        time.sleep(0.4)
        for ch in cmd:
            emit(ch)
            time.sleep(0.045)
        time.sleep(0.35)
        emit("\r\n")
        run(container, cmd, emit)
        emit(PROMPT)
        time.sleep(pause)

    header = {"version": 2, "width": COLS, "height": ROWS, "timestamp": int(time.time()),
              "env": {"TERM": "xterm-256color", "SHELL": "/bin/sh"}}
    with open(out, "w") as f:
        f.write(json.dumps(header) + "\n")
        for e in events:
            f.write(json.dumps(e) + "\n")


def run(container, cmd, emit):
    """Runs cmd in the container through a pseudo-terminal of COLS x ROWS."""
    pid, fd = pty.fork()
    if pid == 0:
        # The sleep keeps the terminal open while the output is read: on macOS the data still
        # in the pseudo-terminal is lost when the process exits.
        os.execvp("docker", ["docker", "exec", "-t", "-w", "/demo/deploys", container, "sh", "-c", cmd + "; sleep 0.5"])
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
    while True:
        r, _, _ = select.select([fd], [], [], 0.1)
        if not r:
            continue
        try:
            data = os.read(fd, 4096)
        except OSError:
            break
        if not data:
            break
        emit(data.decode("utf-8", "replace"))
    os.waitpid(pid, 0)


if __name__ == "__main__":
    main()
