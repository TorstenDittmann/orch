"""Composes the intro's music and sound effects from scratch and writes them to media/.

Everything is synthesized with numpy, so there is nothing to license. The music runs at
120 BPM (a beat every 0.5 s) and its sections start on the video's cuts:

    0.0   Open       pad swells in, riser into the first cut
    3.5   Cluster    kick and bass enter
    15.5  Agents     arpeggio and hats join
    23.5  Batteries  the full groove
    30.5  Outro      final chord, ringing out

Run it from video/intro with `python3 audio/compose.py`; it needs numpy and ffmpeg.
"""

import subprocess
import tempfile
import wave
from pathlib import Path

import numpy as np

RATE = 48000
BEAT = 0.5
LENGTH = 35.4
MEDIA = Path(__file__).resolve().parent.parent / "media"
rng = np.random.default_rng(7)


def hz(midi):
    return 440.0 * 2 ** ((midi - 69) / 12)


def tone(freq, seconds, harmonics=1, rolloff=1.0, detune=0.0):
    """Additive oscillator: `harmonics` partials falling off as 1/n**rolloff."""
    t = np.arange(int(seconds * RATE)) / RATE
    out = np.zeros_like(t)
    for n in range(1, harmonics + 1):
        if freq * n > 16000:
            break
        out += np.sin(2 * np.pi * freq * (1 + detune) * n * t + n * 0.7) / n**rolloff
    return out


def env(n, attack, release, total=None):
    """Linear attack, exponential release envelope over n samples."""
    t = np.arange(n) / RATE
    a = np.clip(t / max(attack, 1e-4), 0, 1)
    r = np.exp(-t / release) if total is None else np.clip((total - t) / release, 0, 1)
    return a * r


def place(track, sound, at, gain=1.0, pan=0.0):
    """Adds a mono `sound` into a stereo `track` at second `at`."""
    i = int(at * RATE)
    if i >= track.shape[1]:
        return
    sound = sound[: track.shape[1] - i]
    left, right = np.sqrt(0.5 * (1 - pan)), np.sqrt(0.5 * (1 + pan))
    track[0, i : i + len(sound)] += sound * gain * left
    track[1, i : i + len(sound)] += sound * gain * right


def lowpass(x, cutoff):
    """Zero-phase FFT low-pass with a soft 1-pole-like slope."""
    spectrum = np.fft.rfft(x)
    freqs = np.fft.rfftfreq(len(x), 1 / RATE)
    spectrum /= np.sqrt(1 + (freqs / cutoff) ** 4)
    return np.fft.irfft(spectrum, len(x))


def highpass(x, cutoff):
    spectrum = np.fft.rfft(x)
    freqs = np.fft.rfftfreq(len(x), 1 / RATE)
    spectrum *= (freqs / cutoff) ** 2 / np.sqrt(1 + (freqs / cutoff) ** 4)
    return np.fft.irfft(spectrum, len(x))


def reverb(stereo, seconds=2.6, mix=0.3):
    n = int(seconds * RATE)
    t = np.arange(n) / RATE
    out = np.empty_like(stereo)
    for ch in range(2):
        ir = rng.standard_normal(n) * np.exp(-t * 6.9 / seconds)
        ir = lowpass(ir, 6000)
        ir /= np.sqrt(np.sum(ir**2))
        size = 1 << int(np.ceil(np.log2(stereo.shape[1] + n)))
        wet = np.fft.irfft(np.fft.rfft(stereo[ch], size) * np.fft.rfft(ir, size), size)
        out[ch] = stereo[ch] * (1 - mix) + wet[: stereo.shape[1]] * mix
    return out


# Am, F, C, G: one chord per bar (2 s), voiced around middle C.
CHORDS = [[57, 60, 64], [53, 57, 60], [48, 52, 55], [55, 59, 62]]
ROOTS = [45, 41, 48, 43]


def chord_at(second):
    return int(second // (4 * BEAT)) % 4


def music():
    track = np.zeros((2, int(LENGTH * RATE)))
    bars = int(LENGTH // (4 * BEAT)) + 1

    # Pad: detuned, soft partials, one chord per bar, swelling in over the open.
    for bar in range(bars):
        start = bar * 4 * BEAT
        if start >= 30.5:
            break
        for k, note in enumerate(CHORDS[bar % 4]):
            for d, pan in ((-0.004, -0.6), (0.004, 0.6)):
                voice = tone(hz(note), 2.6, harmonics=7, rolloff=1.6, detune=d)
                voice *= env(len(voice), 0.5, 0.6, total=2.6)
                place(track, voice, start, gain=0.05, pan=pan * 0.5 + (k - 1) * 0.2)
    swell = np.clip(np.arange(track.shape[1]) / RATE / 3.5, 0, 1) ** 1.5
    track *= 0.35 + 0.65 * swell

    # Riser into the first cut: noise sweeping up.
    rise = rng.standard_normal(int(2.5 * RATE))
    rise = highpass(lowpass(rise, 5000), 600) * np.linspace(0, 1, len(rise)) ** 3
    place(track, rise, 1.0, gain=0.05)

    # Kick on every beat from the cluster on, bass on eighths ducked by the kick.
    kick_t = np.arange(int(0.4 * RATE)) / RATE
    pitch = 45 + 85 * np.exp(-kick_t * 28)
    kick = np.sin(2 * np.pi * np.cumsum(pitch) / RATE) * np.exp(-kick_t * 9)
    hat = highpass(rng.standard_normal(int(0.06 * RATE)), 7000) * np.exp(-np.arange(int(0.06 * RATE)) / RATE * 70)
    beat_times = np.arange(3.5, 30.5, BEAT)
    for i, b in enumerate(beat_times):
        place(track, kick, b, gain=0.55 if b >= 15.5 else 0.42)
        if b >= 15.5:
            place(track, hat, b + BEAT / 2, gain=0.16, pan=0.25)
            if b >= 23.5:
                place(track, hat, b + BEAT * 0.75, gain=0.08, pan=-0.25)
        root = ROOTS[chord_at(b)]
        for half in (0, 1):
            t0 = b + half * BEAT / 2
            if t0 >= 30.5:
                continue
            note = root + (12 if half and i % 4 == 3 else 0)
            bass = tone(hz(note), BEAT / 2, harmonics=3, rolloff=2.0)
            bass *= env(len(bass), 0.02 if half else 0.09, 0.05, total=BEAT / 2)
            place(track, bass, t0, gain=0.28)

    # Arpeggio from the agents scene: chord tones on sixteenths, bouncing left and right.
    step = BEAT / 4
    for i, t0 in enumerate(np.arange(15.5, 30.5, step)):
        notes = CHORDS[chord_at(t0)]
        note = notes[[0, 1, 2, 1][i % 4]] + 12 + (12 if i % 8 == 6 else 0)
        pluck = tone(hz(note), 0.35, harmonics=5, rolloff=1.3) * env(int(0.35 * RATE), 0.003, 0.09)
        gain = 0.07 if t0 < 23.5 else 0.09
        place(track, pluck, t0, gain=gain, pan=0.5 if i % 2 else -0.5)

    # A one-beat break before the outro, then the final chord with a sub drop.
    gap = (np.arange(track.shape[1]) / RATE >= 30.0) & (np.arange(track.shape[1]) / RATE < 30.5)
    track[:, gap] *= np.linspace(1, 0.15, gap.sum())
    for k, note in enumerate([45, 57, 60, 64, 67, 71]):
        voice = tone(hz(note), 5.0, harmonics=8, rolloff=1.4, detune=0.002 * (k - 2))
        voice *= env(len(voice), 0.01, 2.8)
        place(track, voice, 30.5, gain=0.07, pan=(k - 2.5) / 4)
    sub_t = np.arange(int(1.5 * RATE)) / RATE
    sub = np.sin(2 * np.pi * np.cumsum(30 + 60 * np.exp(-sub_t * 6)) / RATE) * np.exp(-sub_t * 2.5)
    place(track, sub, 30.5, gain=0.6)
    place(track, kick, 30.5, gain=0.6)

    track = reverb(track, mix=0.22)
    fade = np.clip((LENGTH - np.arange(track.shape[1]) / RATE) / 2.0, 0, 1)
    return track * fade


def sfx():
    n = lambda s: int(s * RATE)
    t = lambda s: np.arange(n(s)) / RATE
    sounds = {}

    # Key clicks: a short noisy tick with a body, 32 per second with human jitter.
    clicks = np.zeros(n(4.0))
    pos = 0.0
    while pos < 3.9:
        body = highpass(rng.standard_normal(n(0.025)), 1800) * np.exp(-t(0.025) * 260)
        body += np.sin(2 * np.pi * rng.uniform(1700, 2600) * t(0.025)) * np.exp(-t(0.025) * 400) * 0.5
        i = n(pos)
        clicks[i : i + len(body)] += body * rng.uniform(0.6, 1.0)
        pos += 1 / 32 + rng.uniform(-0.008, 0.008)
    sounds["type"] = clicks

    # Pop for things that appear: a short upward blip.
    f = 520 + 420 * (1 - np.exp(-t(0.12) * 40))
    sounds["pop"] = np.sin(2 * np.pi * np.cumsum(f) / RATE) * env(n(0.12), 0.002, 0.035)

    # Whoosh for flights and the cut into a new scene: band-passed noise swelling and fading.
    w = rng.standard_normal(n(0.7))
    w = highpass(lowpass(w, 3500), 400) * np.sin(np.pi * np.linspace(0, 1, n(0.7))) ** 2
    sounds["whoosh"] = w

    # Chime for healthy: two bell partials, a fifth apart.
    bell = sum(np.sin(2 * np.pi * hz(m) * k * t(1.2)) / k**1.5 for m in (88,) for k in (1, 2.76, 5.4))
    sounds["chime"] = bell * env(n(1.2), 0.002, 0.28) * 0.6

    # Tick for packets and tool calls.
    sounds["tick"] = np.sin(2 * np.pi * 3200 * t(0.03)) * np.exp(-t(0.03) * 300)

    # Lock: a small metallic latch, two quick clicks.
    latch = np.zeros(n(0.1))
    for at, freq in ((0.0, 2400), (0.045, 3100)):
        i = n(at)
        latch[i : i + n(0.03)] += np.sin(2 * np.pi * freq * t(0.03)) * np.exp(-t(0.03) * 220)
    sounds["lock"] = latch

    # Error: a low two-step buzz for failures.
    buzz = np.zeros(n(0.42))
    for at, note in ((0.0, 50), (0.2, 47)):
        i = n(at)
        part = tone(hz(note), 0.2, harmonics=9, rolloff=1.0) * env(n(0.2), 0.005, 0.03, total=0.2)
        buzz[i : i + len(part)] += part
    sounds["error"] = lowpass(buzz, 2200)

    # Ping for notifications: a bright two-note rise.
    ping = np.zeros(n(0.6))
    for at, note in ((0.0, 84), (0.07, 91)):
        i = n(at)
        part = tone(hz(note), 0.5, harmonics=3, rolloff=2) * env(n(0.5), 0.002, 0.12)
        ping[i : i + len(part)] += part
    sounds["ping"] = ping
    return sounds


def write_mp3(name, samples, stereo):
    samples = samples / max(np.max(np.abs(samples)), 1e-9) * (0.89 if stereo else 0.7)
    data = (samples.T if stereo else samples)
    pcm = (np.clip(data, -1, 1) * 32767).astype("<i2")
    with tempfile.NamedTemporaryFile(suffix=".wav") as tmp:
        with wave.open(tmp.name, "wb") as w:
            w.setnchannels(2 if stereo else 1)
            w.setsampwidth(2)
            w.setframerate(RATE)
            w.writeframes(pcm.tobytes())
        subprocess.run(
            ["ffmpeg", "-v", "error", "-y", "-i", tmp.name, "-c:a", "libmp3lame", "-b:a", "192k" if stereo else "96k", str(MEDIA / f"{name}.mp3")],
            check=True,
        )


if __name__ == "__main__":
    write_mp3("music", music(), stereo=True)
    for name, samples in sfx().items():
        write_mp3(f"sfx-{name}", samples, stereo=False)
    print("wrote", sorted(p.name for p in MEDIA.glob("*.mp3")))
