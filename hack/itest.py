#!/usr/bin/env python3
"""End-to-end smoke test of GoSlim's proxy mode against a fake LMS.

    python3 hack/itest.py path/to/goslim [-- extra goslim args]

It plays the part of Lyrion (Slimproto server plus HTTP stream server) and of
GoUPnPAudioClient (proxy client), streams a 2 s 44.1 kHz WAV to the player with a
-6 dB ReplayGain, and checks that the audio arrives resampled to 48 kHz and
attenuated, that the right Slimproto status events are sent, that volume is
forwarded, and that SIGTERM shuts the program down cleanly.
"""
import io, json, math, os, signal, socket, struct, subprocess, sys, threading, time, wave

LMS_PORT, PROXY_PORT, HTTP_PORT = 13483, 13434, 13480
exe = sys.argv[1]
extra = sys.argv[sys.argv.index("--") + 1:] if "--" in sys.argv else []

def make_wav(rate=44100, secs=2.0, amp=20000, freq=1000):
    frames = b"".join(struct.pack("<hh", int(amp * math.sin(2 * math.pi * freq * i / rate)),
                                  int(amp * math.sin(2 * math.pi * freq * i / rate)) // 2)
                      for i in range(int(rate * secs)))
    b = io.BytesIO()
    w = wave.open(b, "wb"); w.setnchannels(2); w.setsampwidth(2); w.setframerate(rate); w.writeframes(frames); w.close()
    return b.getvalue()

WAV = make_wav()
# ITEST_FORMAT=aac|alac|ogg streams the same tone as AAC-LC or ALAC in an M4A file, or as Ogg Vorbis (needs ffmpeg
# to make it) instead of the WAV. The track line expected in the verbose log depends on the format.
FORMAT = os.environ.get("ITEST_FORMAT", "wav")
AAC = FORMAT != "wav"
LETTER = {"wav": b"p", "aac": b"a", "alac": b"l", "ogg": b"o"}[FORMAT]
TRACK_LINE = {"wav": "track: WAV PCM, 16-bit, 44.1 kHz, stereo", "aac": "track: AAC LC in MP4, ",
              "alac": "track: ALAC in MP4, 16-bit, 44.1 kHz, stereo", "ogg": "track: Vorbis in Ogg, "}[FORMAT]
if AAC:
    import tempfile
    p = os.path.join(tempfile.mkdtemp(), "t." + {"aac": "m4a", "alac": "m4a", "ogg": "ogg"}[FORMAT])
    codec = {"aac": ["-c:a", "aac", "-b:a", "128k"], "alac": ["-c:a", "alac", "-sample_fmt", "s16p"], "ogg": ["-c:a", "libvorbis", "-q:a", "6"]}[FORMAT]
    subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i",
                    "aevalsrc=0.6104*sin(2*PI*1000*t)|0.3052*sin(2*PI*1000*t):d=2:s=44100"] + codec +
                   (["-movflags", "+faststart"] if p.endswith(".m4a") else []) + [p], check=True)
    WAV = open(p, "rb").read()
events, caps, results = [], {}, {"names": [], "volume": None, "hdr": None, "pcm": bytearray(), "ends": 0}
done = threading.Event()

def recv_exact(s, n):
    buf = b""
    while len(buf) < n:
        c = s.recv(n - len(buf))
        if not c: raise EOFError
        buf += c
    return buf

def http_server():
    srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", HTTP_PORT)); srv.listen(1)
    c, _ = srv.accept()
    c.recv(4096)
    c.sendall(b"HTTP/1.0 200 OK\r\nContent-Type: audio/x-wav\r\n\r\n" + WAV)
    c.close()

def lms_server():
    srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", LMS_PORT)); srv.listen(1)
    c, _ = srv.accept()
    def frame(op, data=b""): return struct.pack(">H", 4 + len(data)) + op + data
    sent = False
    def reader():
        nonlocal sent
        try:
            while True:
                op = recv_exact(c, 4); n = struct.unpack(">I", recv_exact(c, 4))[0]; d = recv_exact(c, n)
                if op == b"HELO":
                    caps["id"], caps["mac"], caps["caps"] = d[0], d[2:8].hex(":"), d[36:].decode()
                    c.sendall(frame(b"setd", b"\x00"))                       # ask for the player name
                    gain = struct.pack(">I", 0x8000)                          # 0.5 linear
                    c.sendall(frame(b"audg", b"\0" * 8 + b"\x01\x00" + gain + gain))
                    hdr = (b"s1" + LETTER + b"????" + bytes([0, 0, 0]) + b"0" + bytes([0, 0, 0]) +
                           struct.pack(">I", 32768) +                         # ReplayGain 0.5 = -6.02 dB
                           struct.pack(">H", HTTP_PORT) + bytes(4))
                    c.sendall(frame(b"strm", hdr + b"GET /stream.wav HTTP/1.0\r\n\r\n"))
                    sent = True
                elif op == b"SETD":
                    results["names"].append(d[1:].rstrip(b"\0").decode())
                elif op == b"STAT":
                    events.append(d[:4].decode())
                    if d[:4] == b"STMu": done.set()
        except EOFError:
            pass
    threading.Thread(target=reader, daemon=True).start()

def proxy_client():
    for _ in range(50):
        try:
            s = socket.create_connection(("127.0.0.1", PROXY_PORT)); break
        except OSError:
            time.sleep(0.1)
    else:
        raise SystemExit("goslim never opened its proxy port")
    def pframe(t, p=b""): return t + struct.pack(">I", len(p)) + p
    s.sendall(pframe(b"H", json.dumps({"version": 1, "mac": "aa:bb:cc:dd:ee:ff", "name": "TestRoom"}).encode()))
    consumed = [0]
    def status():
        while not done.is_set():
            time.sleep(0.25)
            try: s.sendall(pframe(b"S", json.dumps({"state": "playing", "consumed": consumed[0]}).encode()))
            except OSError: return
    threading.Thread(target=status, daemon=True).start()
    try:
        while True:
            t = recv_exact(s, 1); n = struct.unpack(">I", recv_exact(s, 4))[0]; p = recv_exact(s, n)
            if t == b"T": results["hdr"] = p
            elif t == b"D": results["pcm"] += p; consumed[0] += len(p)
            elif t == b"E": results["ends"] += 1
            elif t == b"V": results["volume"] = p[0]
    except (EOFError, OSError):
        pass

signal.alarm(90)  # never hang
threading.Thread(target=http_server, daemon=True).start()
threading.Thread(target=lms_server, daemon=True).start()
time.sleep(0.3)  # let the fake servers start listening
proc = subprocess.Popen([exe, "-proxyport", str(PROXY_PORT), "-server", f"127.0.0.1:{LMS_PORT}", "-webport", "0", "-logtime=false", "-v"] + extra,
                        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
logs = []
threading.Thread(target=lambda: [logs.append(l.rstrip()) for l in proc.stdout], daemon=True).start()
threading.Thread(target=proxy_client, daemon=True).start()

ok = done.wait(20)
time.sleep(0.5)
proc.send_signal(signal.SIGTERM)
try:
    rc = proc.wait(15)
except subprocess.TimeoutExpired:
    proc.kill(); rc = "timeout"
time.sleep(0.2)

pcm = results["pcm"]
samples = struct.unpack("<%dh" % (len(pcm) // 2), bytes(pcm[: len(pcm) // 4 * 4]))
left, right = samples[0::2], samples[1::2]
rms = lambda xs: math.sqrt(sum(x * x for x in xs) / max(1, len(xs)))
hdr = results["hdr"] or b""
rate = struct.unpack("<I", hdr[24:28])[0] if len(hdr) >= 44 else None
frames = len(left)

checks = [
    ("HELO device id 12, caps offer flc,pcm,mp3,aac,alc,ogg and MaxSampleRate=48000", caps.get("id") == 12 and "MaxSampleRate=48000,flc,pcm,mp3,aac,alc,ogg" in caps.get("caps", "")),
    ("player uses the client's MAC", caps.get("mac") == "aa:bb:cc:dd:ee:ff"),
    ("answers the name query with the client's name", results["names"][:1] == ["TestRoom"]),
    ("Slimproto events STMc STMe STMh STMs STMd STMu all sent", all(e in events for e in ("STMc", "STMe", "STMh", "STMs", "STMd", "STMu"))),
    ("stream header says 48000 Hz", rate == 48000),
    ("2 s of 44.1 kHz audio arrived resampled to 48 kHz (~96000 frames)", 95000 <= frames <= 96100),
    ("ReplayGain -6 dB applied (left rms ~ 7071)", 6900 <= rms(left) <= 7250),
    ("right channel kept at half the left level", 0.47 <= rms(right) / max(1, rms(left)) <= 0.53),
    ("volume forwarded (88% for gain 0.5)", results["volume"] == 88),
    ("end-of-track frame sent", results["ends"] >= 1),
    ("SIGTERM exits 0", rc == 0),
    ("log says stopping", any("stopping" in l for l in logs)),
    ("verbose log describes the track and what is done to it", any(
        TRACK_LINE in l and "44.1 kHz, stereo -> decoded, resampled 44.1 -> 48 kHz, ReplayGain -6.02 dB" in l
        or (FORMAT == "wav" and "track: WAV PCM, 16-bit, 44.1 kHz, stereo -> decoded, resampled 44.1 -> 48 kHz, ReplayGain -6.02 dB" in l)
        for l in logs)),
]
print("events:", events)
print("frames:", frames, "left rms %.0f" % rms(left), "right rms %.0f" % rms(right), "rate", rate, "volume", results["volume"], "exit", rc)
bad = [name for name, good in checks if not good]
for name, good in checks:
    print(("PASS  " if good else "FAIL  ") + name)
if bad or not ok:
    print("\n--- program log ---"); print("\n".join(logs))
    sys.exit(1)
