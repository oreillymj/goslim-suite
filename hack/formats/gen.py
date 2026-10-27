#!/usr/bin/env python3
"""Generate the test files for the format-support table in the README.

    python3 hack/formats/gen.py /tmp/fmt [group ...]

Groups: pcm (WAV, AIFF, raw PCM), flac, mp3, aac, alac, ogg. With none given, all
are made. For every format, sample rate and bit depth it writes a 5 s test signal
(two tones plus noise per channel, synthetic, not a real recording) together with
the 16-bit PCM a correct decoder must produce, and a manifest.tsv listing them.
Then run:

    DECODE_MATRIX_DIR=/tmp/fmt FFMPEG=$(which ffmpeg) go test -run FormatMatrix -v ./internal/decode/

Needs Python 3, numpy and ffmpeg (with libmp3lame and libvorbis for the MP3 and
Ogg rows). sox and flac are used if installed, which adds the 8-bit and 32-bit
FLAC rows ffmpeg cannot encode; without them AIFF and FLAC are made by ffmpeg.

"exact" rows are lossless formats compared sample for sample with the source (top
16 bits); "lossy" rows are compared with ffmpeg's own decode of the file.
"""
import os, shutil, subprocess, sys, wave
import numpy as np

OUT = sys.argv[1] if len(sys.argv) > 1 else "/tmp/fmt"
GROUPS = set(sys.argv[2:]) or {"pcm", "flac", "mp3", "aac", "alac", "ogg"}
DUR = 5.0
os.makedirs(OUT, exist_ok=True)
manifest = []
HAVE_SOX, HAVE_FLAC = bool(shutil.which("sox")), bool(shutil.which("flac"))


def sh(*cmd):
    subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def signal(rate, channels=2):
    t = np.arange(int(rate * DUR)) / rate
    rng = np.random.default_rng(1)
    hi = min(7000.0, 0.4 * rate)
    chans = []
    for c in range(channels):
        f1, f2 = 440.0 * (1 + 0.19 * c), hi * (1 - 0.1 * c)
        chans.append(0.35 * np.sin(2 * np.pi * f1 * t) + 0.2 * np.sin(2 * np.pi * f2 * t) + 0.05 * rng.standard_normal(len(t)))
    return np.clip(np.stack(chans, 1), -0.98, 0.98)


def quantize(x, depth):
    return np.round(x * (2 ** (depth - 1) - 1)).astype(np.int64)


def pack(q, depth, big=False):
    """Interleaved integer samples as bytes: unsigned for 8-bit, signed otherwise."""
    if depth == 8:
        return (q + 128).astype(np.uint8).tobytes()
    b = q.astype(">i4" if big else "<i4").tobytes()
    if depth == 32:
        return b
    if depth == 16:
        return q.astype(">i2" if big else "<i2").tobytes()
    return b"".join((b[i + 1:i + 4] if big else b[i:i + 3]) for i in range(0, len(b), 4))


def write_wav(path, q, rate, depth):
    with wave.open(path, "wb") as w:
        w.setnchannels(q.shape[1]); w.setsampwidth(depth // 8); w.setframerate(rate)
        w.writeframes(pack(q, depth))


def ref16(q, depth):
    """What a decoder keeping the top 16 bits must output: stereo, first two channels, mono duplicated."""
    r = q << 8 if depth == 8 else (q if depth == 16 else q >> (depth - 16))
    if r.shape[1] == 1:
        r = np.repeat(r, 2, axis=1)
    return r[:, :2].astype("<i2")


def write_ref(path, q, depth):
    ref16(q, depth).tofile(path)
    return path


def add(name, path, kind, fmt, label, rate, mode, ref, hint=""):
    manifest.append("\t".join([name, os.path.basename(path), kind, fmt, label, str(rate), mode, os.path.basename(ref), hint]))


def source(rate, depth=16, channels=2):
    """A WAV of the test signal; returns (path, quantized samples)."""
    q = quantize(signal(rate, channels), depth)
    p = f"{OUT}/src_{rate}_{depth}_{channels}.wav"
    if not os.path.exists(p):
        write_wav(p, q, rate, depth)
    return p, q


def lossy_ref(path, mono, out, extra=()):
    """ffmpeg's decode of a lossy file as 16-bit stereo (a mono file is duplicated here: ffmpeg's -ac 2 upmix applies -3 dB)."""
    if mono:
        sh("ffmpeg", "-y", "-i", path, *extra, "-f", "s16le", "-ac", "1", "-acodec", "pcm_s16le", out + ".mono")
        m = np.fromfile(out + ".mono", dtype="<i2")
        np.repeat(m[:, None], 2, axis=1).astype("<i2").tofile(out)
    else:
        sh("ffmpeg", "-y", "-i", path, *extra, "-f", "s16le", "-ac", "2", "-acodec", "pcm_s16le", out)


# ---------------------------------------------------------------- WAV, AIFF, FLAC, raw PCM (lossless: compared with the source)
def aiff_from(wav, depth, out):
    if HAVE_SOX:
        sh("sox", "-D", wav, "-e", "signed-integer", "-b", str(depth), out)
    else:
        sh("ffmpeg", "-y", "-i", wav, "-c:a", ("pcm_s8" if depth == 8 else f"pcm_s{depth}be"), "-f", "aiff", out)


def flac_from(wav, channels, out):
    if HAVE_FLAC:
        sh("flac", "-s", "-f", "--lax", *(["--channel-map=none"] if channels > 2 else []), wav, "-o", out)
    else:
        sh("ffmpeg", "-y", "-i", wav, "-c:a", "flac", out)


def lossless(rate, depth, channels=2, tag=""):
    base = f"{rate}_{depth}{tag}"
    q = quantize(signal(rate, channels), depth)
    wav = f"{OUT}/{base}.wav"
    write_wav(wav, q, rate, depth)
    ref = write_ref(f"{OUT}/{base}.ref", q, depth)
    mode = f"{depth}-bit" + {1: " mono", 6: " 5.1"}.get(channels, "")
    if "pcm" in GROUPS:
        add(base + "-wav", wav, "exact", "p", "WAV", rate, mode, ref)
        if channels <= 2:
            aiff = f"{OUT}/{base}.aiff"
            aiff_from(wav, depth, aiff)
            add(base + "-aiff", aiff, "exact", "p", "AIFF", rate, mode, ref)
    if "flac" in GROUPS and (HAVE_FLAC or depth in (16, 24)):  # ffmpeg's FLAC encoder silently makes 8-bit 16-bit and 32-bit 24-bit
        flac = f"{OUT}/{base}.flac"
        try:
            flac_from(wav, channels, flac)
            add(base + "-flac", flac, "exact", "f", "FLAC", rate, mode, ref)
        except subprocess.CalledProcessError:
            print("could not make FLAC for", base, file=sys.stderr)


if GROUPS & {"pcm", "flac"}:
    for rate in (44100, 48000, 88200, 96000, 176400, 192000):
        for depth in (16, 24, 32):
            lossless(rate, depth)
    lossless(48000, 8)
    lossless(48000, 16, channels=1, tag="_mono")
    lossless(48000, 24, channels=6, tag="_51")

if "pcm" in GROUPS:
    # WAV with 32-bit float samples: not supported, the table should say so.
    q16 = quantize(signal(48000), 16)
    write_wav(f"{OUT}/tmp16.wav", q16, 48000, 16)
    wavf = f"{OUT}/48000_float.wav"
    if HAVE_SOX:
        sh("sox", "-D", f"{OUT}/tmp16.wav", "-e", "floating-point", "-b", "32", wavf)
    else:
        sh("ffmpeg", "-y", "-i", f"{OUT}/tmp16.wav", "-c:a", "pcm_f32le", wavf)
    add("float-wav", wavf, "exact", "p", "WAV", 48000, "32-bit float", write_ref(f"{OUT}/48000_float.ref", q16, 16))

    # Raw PCM has no header: LMS describes it in the strm command (sample size, rate, channels, endianness).
    def raw(name, rate, depth, channels, big, hint):
        q = quantize(signal(rate, channels), depth)
        f = f"{OUT}/{name}.raw"
        open(f, "wb").write(pack(q, depth, big))
        mode = f"{depth}-bit" + (" mono" if channels == 1 else "") + (", big-endian" if big else "")
        add(name, f, "exact", "p", "Raw PCM (no header)", rate, mode, write_ref(f"{OUT}/{name}.ref", q, depth), hint)

    raw("raw-16-44k", 44100, 16, 2, False, "1321")
    raw("raw-16-48k-be", 48000, 16, 2, True, "1420")
    raw("raw-24-48k", 48000, 24, 2, False, "2421")
    raw("raw-32-96k", 96000, 32, 2, False, "3921")
    raw("raw-8-22k", 22050, 8, 2, False, "0121")
    raw("raw-16-44k-mono", 44100, 16, 1, False, "1311")


# ---------------------------------------------------------------- MP3 (lossy: compared with ffmpeg's decoder)
def mp3(rate, label, args):
    src, _ = source(rate)
    f = f"{OUT}/{rate}_{label}.mp3"
    # no Xing/LAME header, so ffmpeg doesn't trim the encoder delay and both decoders output the same samples
    sh("ffmpeg", "-y", "-i", src, "-c:a", "libmp3lame", *args, "-write_xing", "0", f)
    ref = f"{OUT}/{rate}_{label}.ref"
    lossy_ref(f, "-ac" in args, ref)
    add(f"mp3-{rate}-{label}", f, "lossy", "m", "MP3", rate, label, ref)


if "mp3" in GROUPS:
    for rate in (48000, 44100, 32000):
        mp3(rate, "128 kbps CBR", ["-b:a", "128k"])
        mp3(rate, "320 kbps CBR", ["-b:a", "320k"])
        mp3(rate, "VBR V0", ["-q:a", "0"])
    mp3(44100, "128 kbps mono", ["-b:a", "128k", "-ac", "1"])
    for rate in (24000, 22050, 16000):
        mp3(rate, "64 kbps CBR (MPEG-2)", ["-b:a", "64k"])
        mp3(rate, "160 kbps CBR (MPEG-2)", ["-b:a", "160k"])
    for rate in (12000, 11025, 8000):
        mp3(rate, "32 kbps CBR (MPEG-2.5)", ["-b:a", "32k"])


# ---------------------------------------------------------------- AAC (lossy: compared with ffmpeg's decoder)
def aac(name, label, rate, args, ext, channels=2):
    src, _ = source(rate, 16, channels)
    f = f"{OUT}/{name}.{ext}"
    sh("ffmpeg", "-y", "-i", src, "-c:a", "aac", *args, f)
    ref = f"{OUT}/{name}.ref"
    # ffmpeg does not trim the encoder padding at the end of an m4a; the edit list says where the audio ends.
    lossy_ref(f, channels == 1, ref, ["-af", f"atrim=end_sample={int(rate * DUR)}"] if ext == "m4a" else [])
    bitrate = args[args.index("-b:a") + 1].replace("k", " kbps") if "-b:a" in args else "VBR"
    add(name, f, "lossy", "a", label, rate, f"AAC-LC {'mono' if channels == 1 else 'stereo'}, {bitrate}", ref)


if "aac" in GROUPS:
    aac("aac-adts-48", "AAC (ADTS .aac)", 48000, ["-b:a", "128k"], "aac")
    aac("aac-adts-44", "AAC (ADTS .aac)", 44100, ["-b:a", "256k"], "aac")
    aac("aac-m4a-front-48", "AAC in M4A, index first", 48000, ["-b:a", "128k", "-movflags", "+faststart"], "m4a")
    aac("aac-m4a-last-44", "AAC in M4A, index last", 44100, ["-b:a", "192k"], "m4a")
    aac("aac-m4a-vbr-44", "AAC in M4A, index first", 44100, ["-q:a", "2", "-movflags", "+faststart"], "m4a")
    aac("aac-m4a-mono-44", "AAC in M4A, index last", 44100, ["-b:a", "64k"], "m4a", channels=1)
    aac("aac-m4a-96", "AAC in M4A, index last", 96000, ["-b:a", "256k"], "m4a")
    aac("aac-m4a-32", "AAC in M4A, index last", 32000, ["-b:a", "96k"], "m4a")
    aac("aac-m4a-22", "AAC in M4A, index last", 22050, ["-b:a", "48k"], "m4a")
    aac("aac-m4a-8", "AAC in M4A, index last", 8000, ["-b:a", "24k"], "m4a")


# ---------------------------------------------------------------- ALAC (lossless: compared with the source)
def alac(name, label, rate, depth, channels, extra=()):
    src, q = source(rate, depth, channels)
    f = f"{OUT}/{name}.m4a"
    sh("ffmpeg", "-y", "-i", src, "-c:a", "alac", "-sample_fmt", "s16p" if depth == 16 else "s32p", *extra, f)
    mode = f"{depth}-bit" + (" mono" if channels == 1 else "")
    add(name, f, "exact", "l", label, rate, mode, write_ref(f"{OUT}/{name}.ref", q, depth))


if "alac" in GROUPS:
    alac("alac-16-44-last", "ALAC in M4A, index last", 44100, 16, 2)
    alac("alac-16-44-front", "ALAC in M4A, index first", 44100, 16, 2, ["-movflags", "+faststart"])
    alac("alac-16-48-last", "ALAC in M4A, index last", 48000, 16, 2)
    alac("alac-24-48-last", "ALAC in M4A, index last", 48000, 24, 2)
    alac("alac-24-96-front", "ALAC in M4A, index first", 96000, 24, 2, ["-movflags", "+faststart"])
    alac("alac-24-192-last", "ALAC in M4A, index last", 192000, 24, 2)
    alac("alac-16-44-mono", "ALAC in M4A, index last", 44100, 16, 1)
    alac("alac-24-96-mono", "ALAC in M4A, index last", 96000, 24, 1)
    alac("alac-16-8", "ALAC in M4A, index last", 8000, 16, 2)
    alac("alac-16-44-uncompressed", "ALAC in M4A, index last", 44100, 16, 2, ["-compression_level", "0"])


# ---------------------------------------------------------------- Ogg Vorbis (lossy: compared with ffmpeg's decoder)
def ogg(name, rate, channels, args, mode):
    src, _ = source(rate, 16, channels)
    f = f"{OUT}/{name}.ogg"
    sh("ffmpeg", "-y", "-i", src, "-c:a", "libvorbis", *args, f)
    ref = f"{OUT}/{name}.ref"
    lossy_ref(f, channels == 1, ref)
    add(name, f, "lossy", "o", "Ogg Vorbis", rate, mode, ref)


if "ogg" in GROUPS:
    ogg("ogg-44-q5", 44100, 2, ["-q:a", "5"], "stereo, q5 (about 160 kbps)")
    ogg("ogg-48-q10", 48000, 2, ["-q:a", "10"], "stereo, q10 (about 500 kbps)")
    ogg("ogg-44-q0", 44100, 2, ["-q:a", "0"], "stereo, q0 (about 64 kbps)")
    ogg("ogg-44-128k", 44100, 2, ["-b:a", "128k"], "stereo, 128 kbps")
    ogg("ogg-44-mono", 44100, 1, ["-q:a", "4"], "mono, q4")
    ogg("ogg-96-q6", 96000, 2, ["-q:a", "6"], "stereo, q6")
    ogg("ogg-32-q4", 32000, 2, ["-q:a", "4"], "stereo, q4")
    ogg("ogg-22-q3", 22050, 2, ["-q:a", "3"], "stereo, q3")
    ogg("ogg-8-q3", 8000, 2, ["-q:a", "3"], "stereo, q3")

open(f"{OUT}/manifest.tsv", "w").write("\n".join(manifest) + "\n")
print(len(manifest), "test files in", OUT)
