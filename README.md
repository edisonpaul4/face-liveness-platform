# Face Liveness Platform

Active **face liveness detection** (PAD, *Presentation Attack Detection*),
similar in scope to AWS Rekognition Face Liveness. The client opens a session,
the server imposes challenges one at a time, the client streams live camera
frames over WebSocket, and the server decides whether a **real person is
present** in front of the camera at that moment.

Each session ends in `LIVE` / `SPOOF` / `INCONCLUSIVE`, a confidence score and
an auditable evidence package.

> **Status: research demo.** It runs end to end and has been tuned against real
> sessions, but it is **not calibrated against real attacks** and is not
> production-certified. See [Known gaps](#known-gaps).
>
> The normative design document is [`CLAUDE.md`](CLAUDE.md) and the measured
> bench results are in [`bench/RESULTS.md`](bench/RESULTS.md). Both are written
> in Spanish. If this README and `CLAUDE.md` disagree, `CLAUDE.md` wins.

---

## How a session works

1. `POST /v1/sessions` creates the session and returns a single-use token.
2. The client opens the WebSocket at `/v1/liveness` and starts sending JPEG
   frames: 480p, up to 30 fps, capped at 9 Mbit/s.
3. The server draws a random script and reveals it **one step at a time**:
   - **Calibration**: neutral screen, captures the subject's baseline.
   - **Head poses**: turn left, turn right, chin up.
   - **Gaze**: a dot appears on one side, jumps to the other, and the eye
     movement between both is measured.
   - **Hold still** for 11 s: the window where the pulse (rPPG) is measured.
   - **Move closer**: always right before the flashes.
   - **Flashes**: the screen is tinted with two saturated colors of random
     duration, and the skin's response is measured.
4. The analyzer measures every frame and every window. The gateway fuses the
   measurements with the decision profile and issues the verdict.

The critical rule: **the client never learns the script**. Not how many steps
there are, not which comes next, not the seed, not the thresholds. That is what
stops a pre-recorded video from passing.

---

## Architecture

```
 browser ──WSS──► gateway (Go) ◄─in-proc─► orchestrator (Go)
                      │                       │    │    │
                      │ NATS core             Redis  PG  MinIO
                      ▼
                 analyzer (Python)
                 vision + signal → metrics ONLY
```

| Path | Language | Role |
|---|---|---|
| `gateway/` | Go 1.23 | WebSocket, session auth, backpressure, bus, challenge delivery, gaze and capture-quality evaluation. |
| `orchestrator/` | Go 1.23 | State machine, challenge script, score fusion, verdict, persistence, control API. |
| `analyzer/` | Python | Face landmarks, photometry, pose, gaze, rPPG, passive PAD. Returns named numbers, never judgments. |
| `web/` | Plain JS | Minimal test client: camera, challenge rendering, session recording. |
| `frontend/` | SvelteKit | Real client application. |
| `bench/` | Python | Attack bench, APCER/BPCER metrics, replay of recorded sessions. |
| `proto/` | protobuf / JSON Schema | Single source of truth for bus and client protocol contracts. |
| `deploy/` | — | Local infrastructure and the decision profile. |

### Infrastructure

| Service | Use |
|---|---|
| NATS core, no JetStream | Go ⇄ Python bus. A lost frame is never retried. Session affinity through a lease with heartbeat. |
| Redis | Hot session state, always with a TTL. |
| Postgres | Results and append-only audit, enforced by triggers. |
| MinIO | Evidence encrypted client-side with AES-256-GCM, deleted after 24 h. |

### The four non-negotiable rules

1. **Go decides, Python measures.** The analyzer knows nothing about sessions,
   challenges or verdicts. The wire codec rejects signals with judgment names
   such as `is_live` or `spoof_score`.
2. **The client never knows the challenge script.** One challenge at a time,
   no index, no total, no future.
3. **The frame bus is ephemeral.** If an analyzer worker dies, the session is
   aborted and restarted from scratch with a new ticket and script.
4. **Biometrics expire, the audit stays.** Frames at 24 h, features at 72 h.
   The verdict and its reasons are kept without anyone's face. Deletion is
   automatic and verified, with a receipt.

---

## Signals and what they catch

| Signal | Catches |
|---|---|
| `flash_gradient_3d` | Printed photo: it follows the flash, but flat, with no relief. |
| `flash_correlation` | Video replay: a recording cannot follow colors emitted now. |
| `flash_screen_absence` | Screen: self-emitting, moiré, refresh banding. |
| `pose_parallax` | Flat surface in motion: a homography explains its rotation exactly. |
| `gaze_response` | Video replay: it cannot look at a dot that just appeared. |
| `rppg_snr` | Mask: silicone has no pulse. |
| `texture_pad_v2`, `texture_pad_v1se` | Passive single-frame texture PAD. |
| `pose_continuity`, `temporal_plausibility` | Video cuts and faster-than-human responses. |

### Fusion

The profile lives in [`deploy/policy/decision-profile.yaml`](deploy/policy/decision-profile.yaml).
It hot-reloads, and its version travels with every result.

- Weighted mean of the measured signals, with hand-set weights.
- Pass from 0.75, reject below 0.45, retry in between.
- Per-signal floors that veto on their own, e.g. 3D gradient below 0.30.
- Up to 3 attempts with 5 s, 30 s and 120 s backoff.

Three principles run through everything:

- **What could not be measured does not enter the fusion.** It never counts as zero.
- **Bad quality is not an attack.** No quality path can end in a rejection.
- **The pulse can acquit but never accuse**, because its signal depends on skin tone.

---

## Models and algorithms

### In use

All run on CPU, one thread per process, within a 30 ms per-frame budget.
They are downloaded with `make models` and not versioned.

| Model | File | Purpose | Source |
|---|---|---|---|
| MediaPipe Face Landmarker | `face_landmarker.task` | Default detector. 478 face points including iris, plus head pose matrix. | Google MediaPipe |
| YuNet | `face_detection_yunet_2023mar.onnx` | Alternative detector via ONNX Runtime, swappable by config. | OpenCV Zoo |
| SFace | `face_recognition_sface_2021dec.onnx` | 128-d embedding every 3 frames for identity continuity. It identifies no one and compares against no database. | OpenCV Zoo |
| MiniFASNet V2 | `MiniFASNetV2.onnx` | Passive texture PAD. Weight 0.06. | yakhyo/face-anti-spoofing, Apache-2.0 |
| MiniFASNet V1SE | `MiniFASNetV1SE.onnx` | Second passive PAD variant. Weight 0.02. | yakhyo/face-anti-spoofing, Apache-2.0 |

MediaPipe is pinned to the 0.10 line, because 1.0 aborts the process on macOS
ARM when initializing Metal. When the detector misses a face, it retries on the
image with a 15 % replicated border, which raises detection of faces touching
the frame edges from 57 % to 98 %.

### Algorithms without neural networks

| Measurement | Method |
|---|---|
| Flash | Per-region face photometry divided by the background of the same frame, relative to calibration. Correlated against the emitted sequence with a 0–600 ms lag search, weighted by each region's amplitude. |
| 3D gradient | Dispersion of the flash response across forehead, cheeks and nose. |
| Parallax | Residual of a homography fitted on landmarks during the head turn. A plane is explained exactly. |
| Gaze | Iris position between the eye corners, converted to degrees at 248°/unit and added to head yaw. |
| Pulse | POS method on skin color divided by background, FFT over 10 s or more. |
| Screen | Moiré and banding indices, specular highlights, face-to-background brightness. |
| Sensor | Rolling shutter in the transition frame and the camera's auto-exposure reaction. Logged, no vote yet. |

### Tried and discarded

| Model | Why |
|---|---|
| anti-spoof-mn3, OpenVINO | Seven times the size of MiniFASNet and did not beat it on our bench despite its published figure. |
| FLIP, fine-tuned CLIP ViT-B/16 | AUC 0.88 vs 0.93 for MiniFASNet V2, and worse than untuned CLIP. 45 ms per pass. |
| Equal-weight V2 + V1SE | Both see the same crop, so it counted one measurement twice. Combined they score worse than V2 alone. |

---

## Getting started

Requires Docker. Go and Python only for the components you touch; targets skip
missing toolchains with a warning.

```bash
cp .env.example .env
make up          # NATS, Redis, Postgres, MinIO
make venv        # analyzer virtualenv
make models      # detection and PAD models
make test        # tests for every component
```

Run the stack:

```bash
make analyzer    # analysis workers
make gateway     # 480p@30 by default · CAPTURE=720 · EXPLAIN=1 pentest mode
make web         # test client at http://localhost:5173
make help        # every other target
```

To test from a phone, open two HTTPS tunnels, one to the client and one to the
gateway. Add the client tunnel's origin to `GATEWAY_ALLOWED_ORIGINS` and point
the client at the gateway tunnel:

```bash
node web/serve.mjs 5173 --gateway https://<gateway-tunnel>
```

### Pentest mode

`EXPLAIN=1` sets `GATEWAY_INSECURE_EXPLAIN_VERDICT`: the client receives the
per-detector breakdown with value, floor and pass/fail. Testing only. In
production the client gets nothing more than `try_again`, because telling an
attacker which detector caught them is handing over the feedback loop.

### Control API

```
POST /v1/sessions                     create session → sessionId + WebSocket token
GET  /v1/sessions/{id}                result, reasons and biometric data status
GET  /v1/sessions/{id}/timeline       features (410 Gone once expired)
GET  /v1/sessions/{id}/retention      deletion receipts
GET  /v1/sessions?outcome=&subject_id=&reason_code=&from=&to=   listing
```

All under `Authorization: Bearer <key>`.

---

## How it is tested and tuned

The method never changes: **measure, form a hypothesis, try to refute it with
data, and only then change code**. Every decision is written down next to the
measurement that justifies it.

### Three levels

1. **Automated tests.** `make test` runs about 245 Go tests, 159 Python tests
   and 19 JS tests. Several are property tests over thousands of seeds, e.g.
   that the script never leaks future steps or that flash durations always
   stay apart.
2. **Synthetic end-to-end bench.** `make bench-web` plays full sessions
   against the real gateway using the browser's own modules, with rendered 3D
   heads as the camera.
3. **Real sessions from a phone.** Full sessions through the tunnels with
   pentest mode on, then the gateway log and detector table are read.

### Attack datasets

| Dataset | Content | Use |
|---|---|---|
| AxonData face-anti-spoofing | 29 bona fide and 120 attacks of eight types, mostly masks. | First APCER/BPCER matrix. |
| CASIA-FASD | 8,126 images of 30 subjects: bent photos, cut photos, screen replay. | Covers paper and screens. Internal evaluation only, per its license. |

Passive detectors are measured with `make bench-dataset`, picking the threshold
on one half and measuring on the other, split by subject. Texture catches paper
and misses screens; moiré and banding do the opposite. Thresholds do not
transfer between datasets, which is why passive PAD has no veto.

### The recorded-session loop

The test client records every session: a JSON header with the revealed
challenges, per-frame capture timestamps, paint times, camera settings and
drops, followed by the JPEG frames.

1. A session is played on a phone and recorded.
2. `make replay REC=<file>` and analysis scripts recompute every window
   offline with the production code.
3. Each measurement is checked against **controls**: the reversed sequence,
   and the same sequence laid over a window with no flash. A measure that
   scores the same on the control measures nothing.
4. If the hypothesis survives, the code or profile changes, a test is added
   with the real case, and the session is repeated.

Recordings are **real biometric data**. They stay on the recorder's machine,
are git-ignored, and are deleted once the tuning they were taken for is done.

---

## What the measurements taught us

- **480p at 30 fps, JPEG, encoded in a Worker.** At 720p the iris measured
  worse and the uplink saturated. WebP was half the size but took 28 ms per
  frame and sank the frame rate.
- **Native camera aspect ratio.** Phones deliver portrait frames, and faces
  were being stretched 1.78×. Fixing it required re-deriving the gaze constant.
- **Cap by bitrate, not frame rate.** Exceeding the link does not drop frames,
  it delays them, and a late frame lies about when its content happened.
- **Gaze searches its own lag** up to 0.8 s, and a transition with no frames
  around it is reported as unmeasurable rather than as a null response.
- **Two-color flashes, no white, 350–600 ms, with durations at least 100 ms
  apart.** Longer segments let the camera's white balance cancel the color.
  Similar durations let the reversed sequence fit just as well.

### Flash in daylight

At night, all measured flash windows correlate between 0.92 and 0.99. In
daylight they fail: the camera exposes 5–10× shorter and ambient light buries
the flash. It is not an algorithm problem; eight alternative estimators were
tested against controls and none discriminates. AWS has the same limitation on
the web and asks users to set screen brightness to maximum manually.

---

## Known gaps

- APCER against real attacks performed in front of the camera: printed photo,
  screen replay, mask.
- Calibration of the emissive-surface cues against real screens.
- Gaze thresholds still live in Go code instead of the decision profile.
- Passive PAD and pulse must be measured per skin tone before getting more weight.
- `ANALYZER_MIN_FACE_CONFIDENCE` is not passed to MediaPipe.
- Camera injection and real-time deepfakes are out of scope.

---

## License

[MIT](LICENSE). Downloaded models keep their own licenses. Bench datasets are
not included and are subject to their providers' terms.

Author: Paul Mosquera · [linkedin.com/in/paul-mosquera](https://www.linkedin.com/in/paul-mosquera/)
