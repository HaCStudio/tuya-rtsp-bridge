"""Optional local archive of HD RTSP. Off by default. No hardcoded cameras."""
from __future__ import annotations

import json
import os
import subprocess
import time
import urllib.request
from paths import ffmpeg_exe, user_data

ARCH = user_data() / "archive"
LOG = ARCH / "recorder.log"
SEG_SEC = 300


def log(msg: str) -> None:
    ARCH.mkdir(parents=True, exist_ok=True)
    with LOG.open("a", encoding="utf-8") as fh:
        fh.write(time.strftime("%Y-%m-%d %H:%M:%S ") + msg + "\n")


def cameras() -> list[str]:
    try:
        st = json.loads(urllib.request.urlopen("http://127.0.0.1:8787/api/state", timeout=5).read())
    except Exception:
        return []
    names = []
    for cam in st.get("cameras") or []:
        url = cam.get("rtspHd") or ""
        if "/hd" in url:
            names.append(url.rsplit("/", 2)[-2])
    return names


def main() -> None:
    ff = ffmpeg_exe()
    if not ff:
        log("ffmpeg missing — archive idle")
        while True:
            time.sleep(3600)
    flags = subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0
    procs: dict[str, subprocess.Popen] = {}
    while True:
        for cam in cameras():
            p = procs.get(cam)
            if p and p.poll() is None:
                continue
            dest = ARCH / cam
            dest.mkdir(parents=True, exist_ok=True)
            out = str(dest / f"{cam}_%Y%m%d_%H%M%S.ts")
            procs[cam] = subprocess.Popen(
                [
                    str(ff), "-hide_banner", "-loglevel", "error",
                    "-rtsp_transport", "tcp", "-timeout", "15000000",
                    # The RTSP description omits codec headers. Allow time for
                    # in-band headers to arrive before opening the segment muxer.
                    "-analyzeduration", "15000000", "-probesize", "20000000",
                    "-i", f"rtsp://127.0.0.1:8554/{cam}/hd",
                    "-map", "0:v:0", "-c", "copy", "-an",
                    "-f", "segment", "-segment_time", str(SEG_SEC),
                    "-reset_timestamps", "1", "-strftime", "1", out,
                ],
                cwd=str(ARCH),
                creationflags=flags,
            )
            log(f"record {cam}")
        time.sleep(20)


if __name__ == "__main__":
    main()
