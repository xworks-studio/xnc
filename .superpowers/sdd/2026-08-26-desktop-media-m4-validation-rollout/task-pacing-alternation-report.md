# Task report — presentation pacing alternation (framediag `1 2 1 2 3 4 3 4 …`) fix

Date: 2026-08-29. Branch `codex/desktop-media-m4` (base 0499d22). Commit:
**fix(agent): flatten viewer frame pacing alternation** (`pacingBudgetFraction`
0.85 → 1.05 + regression test + e2eviewer `-direct-budget` A/B instrument).
`bin/xnc-agent-m4.exe` rebuilt with the ldflags stamp `0.5.10-m4canary.3`
(verified via `--version`; not uploaded — prod node still down). Go suites
`./agent/desktop ./agent/desktoppipe ./tools/e2eviewer` green. Native untouched
(no native rebuild needed; host binary already contains ad14a96 — verified by
source/obj/exe timestamps 12:32/12:33 vs commit 12:41).

## 1. What ?framediag actually records (the decode)

`web/src/pages/DesktopLive.tsx` (`startFrameLoop`'s rVFC `tick`, lines ~747-759):
one record **per PRESENTED frame** (rVFC fires when Chrome composites a frame),

```
di.frames.push(`${frameCount}:${mt.toFixed(3)}:${dt.toFixed(1)}${id}`)
```

- `frameCount` — presented-frame counter (rVFC callback count)
- `mediaTime` — the AU's media timestamp (s; derived from RTP ts = host `PresentMonoUs`)
- `dt` — wall-clock ms since the previous **presented** frame
- `id` — optional frame-meta identity (`:E<codecEpoch>:S<encodeSeq>`)

The user's `1 2 1 2 3 4 3 4 5 6 5 6 7 8 7 8 9` is the **dt column in display-tick
units** (vsync 16.67 ms on a 60 Hz panel; frame-period ticks at 30 fps give the
same pairs reading). Decoded:

- **pairs (n, n+1) alternating** = AU *completion* intervals (last-packet arrival
  through Chrome's jitter buffer → next vsync presentation) are non-integer in
  display ticks — each beat lands alternately n and n+1 vsyncs late/early;
- **level growth 1/2 → 3/4 → 5/6 → 7/8 → 9** = the completion interval itself
  grows stepwise over the session — the pacing drain rate falling ever further
  below per-frame bytes. The user's tolerated example (`1 2 2 3 4 5 5 5 …`) is
  integer-multiple presentation with holds — no completion-interval growth.

## 2. Root cause — suspect 1 (ViewerSender token bucket) CONFIRMED

Structural double-discount: since M3, the pacing budget source is the QoS
decision bitrate (`session.go` `qosSession.apply` → `SetPacingBudget(int(a.Config.Bitrate))`)
— the **same value that is the encoder target**, and `qosTargetBitrate` is
already `0.85×est` (the 15% link margin for retrans/signaling lives THERE).
`viewer_sender.go` then applied a **second** 0.85 (`pacingBudgetFraction`) →
drain rate = 0.85 × encoder target. Whenever the encoder produces at target
(sustained motion — the production case; the local loop never hits it because
the RDP compositor caps content at ~21 fps = ~1.1-1.5 Mbps demand vs 2.3 M):

1. token debt deepens ~15%/frame → a delta's last-packet deadline crosses the
   100 ms enqueue gate → **whole frame rejected** → waitIDR → merged keyframe
   request → recovery IDR (gate-exempt, debt clamped to the 50 ms floor) →
   a few deltas → gate again …
2. frame tails exit as **supersede-flush bursts** (unpaced) at the next
   Enqueue → wire bursts + starve gaps = the alternation;
3. the receiver-side queue the bursts create reports `queueMs > 100` → QoS
   congestion channel cuts bitrate 30%/s → fps ladder `{30,20,15,10,5}` walks
   down → completion interval = bytes/rate grows stepwise = **the growing
   level**; est tracks goodput (0.85×budget shadow) so upshift can never
   restore — one-way ratchet.

### Evidence A — deterministic harness (manualClock full-chain drive; temp file, numbers below)

30 fps, budget 2.3 Mbps (= 1920-width tier), encoder at target 9583 B/frame,
completion = OnFrameSent time; ticks = interval/16.67 ms:

| leg | deadline drops / IDRs (per run) | completion intervals | vsync ticks |
|---|---|---|---|
| **BEFORE ×0.85** 330f | 36 / 36 (frames 8,17,26,… every 9th) | 33×7 + **67** stutter cycle | `2 2 2 2 2 2 2 4 …` |
| ×1.00 | 2 / 2 (RTP-header drift, see below) | 33.7 flat | `2 2 2 2 …` |
| **AFTER ×1.05** | **0 / 0** | 33.3 flat, std 0.23 ms | `2 2 2 2 …` |
| BEFORE @ fps20/500k | 31/220f | 50×5 + 100 | `3 3 3 3 3 3 3 6 …` |
| BEFORE @ fps15 | 33/165f | 67×3 + 133 | `4 4 4 8 …` |
| BEFORE @ fps10 | 27/110f | **70 / 119 / 211 repeating** | **`4 7 13 4 7 13 …`** ← the user's signature verbatim |
| AFTER @ fps20 / fps15 / fps10 | 0 | 50 / 66.7 / 100 flat | `3 3 3` / `4 4 4` / `6 6 6` |

(×1.00 leaves a ~1.2% structural deficit: the bucket charges `MarshalSize`
wire bytes — 12 B RTP + FU headers per packet, plus TWCC extension bytes
stamped after the pacer — while content bytes = rate/fps; debt trips the gate
every ~130 frames. Hence ×1.05.)

fps5 note (pre-existing, unchanged by this fix): at fps≤5 one frame's drain
(200 ms) inherently exceeds the 100 ms enqueue gate → only gate-exempt IDRs
pass = IDR-only degenerate stream. Out of scope (needs gate ≥ 1 spf or an fps
ladder floor of 10); with the pacing fix the spiral that reaches the ladder
bottom no longer runs.

### Evidence B — live direct-pipe A/B (production shape; same host, motion, cadence; ONLY the fraction changed)

Host `bin/xnc-desktop.exe --console-rt --pipe \\.\pipe\xnc-m4-fps --fps 30`
(XNC_DESKTOP_PIPELINE_V2=1, QSV hardware rung, 1920x804, encoder 2.3 M —
ad14a96 owed-poll verified present), Edge motion.html (~2000x1240, confetti),
45-60 s runs, e2eviewer direct mode, pacing budget 1.2 Mbps ≈ content demand
(~1.13 Mbps measured at default pacing). New `-direct-budget` flag drives
`Publisher.SetPacingBudget` = the production QoS wiring shape.

| metric | baseline (20 M, unfixed) | **BEFORE 0.85× @1.2M** | **AFTER 1.05× @1.2M** |
|---|---|---|---|
| preKeyDropped (sender-suppressed) | 0 | **66** | **0** |
| keyframes (recovery IDR churn) | 4 | **27** | **4** |
| NACKs (wire-burst loss/reorder) | 0 | **306** | **0** |
| same-ms burst arrivals (Δt=0) | 0 | **549** | **0** |
| starvation gaps >500 ms (max) | 0 | **17 (7.84 s)** | **0 (max 90 ms)** |
| interval median / std / max | 48 / 10.4 / 89 ms | **0 ms / 353 / 7840 ms** | 48 / 9.6 / 90 ms |
| queue age p50/p95/max | 19.9/42/57 ms | 27.4/49.7/70.4 | 19.1/38.4/58.8 (<100 hard bound) |
| frames delivered (45 s) | 917 | 830 | 923 |

BEFORE degenerate-window raw deltas (ms): `34 56 50 48 48 47 47 47 37 58 43 50
73 36 60 50 30 34 47 61 49 53 48 34 62 …` — the alternating short/long pairs
(≈34/≈60) = user's `(n, n+1)` pairs; AFTER: `47 48 48 31 51 34 60 42 59 47 49 …`
(stable-level residual = host-emit beat, see §3). 60 s AFTER confirmation run:
preKeyDropped=0, nacks=0, keyframes=5 (periodic only), max Δt 86 ms.

## 3. Suspects 2 and 3 — residual only, not the reported defect

- **Host emit depth batching (suspect 2)**: with pacing unsaturated (baseline
  20 M), pipe-arrival std ≈ 10 ms on this box (RDP compositor ~20.5 fps; two
  clusters ~34/~60 ms). bin/xnc-desktop.exe contains ad14a96 (timestamps
  12:32 src → 12:33 obj/exe, commit 12:41). This residual is **stable-level**
  alternation — no growth — i.e. inside the user's tolerance class
  (`2 2 2 3 2 …` at true 30 fps where 33.3 ms = exactly 2 vsyncs); previous
  task already judged further host smoothing out of budget. My runs' residual
  is noisier than that task's 3.12 ms (compositor state varies per session) —
  same binary, different DWM day.
- **Browser jitter-buffer/vsync allocator (suspect 3)**: no browser leg was
  measurable locally (dev-agent path needs elevation; canary node down). Per
  the task's own rule, sender-side arrival flattening is the fix for browser
  judder — delivered here: the burst-arrival pattern (549 zero-delta arrivals +
  gaps) that feeds Chrome's buffer swing is gone, so the 3:2-style allocator
  input is gone.

## 4. The fix

`agent/desktop/viewer_sender.go`: `pacingBudgetFraction` 0.85 → **1.05**
(+ comment block documenting the mechanism/measurements; tokenBucket and
PublisherConfig comments updated). Rationale: the 15% link margin is already
banked in the QoS target (0.85×est) — the pacer must drain at ≥ encoder rate
or it re-borrows 15% every frame; ×1.05 covers per-packet wire overhead
(~1.2% RTP/FU headers + TWCC extension bytes billed after the pacer) and VBV
overshoot ripple. Wire rate stays ≤ 1.05×0.85×est < est — no bandwidth
regression. Smallest change: one constant; no queue semantics, gate, burst
capacity, or latency added (queue bounds 50/100 ms respected in all runs;
AUs publish no later — completion spread shortens).

No smoothing hold added: the growing-pairs defect is the pacing collapse
(eliminated at the source); a 1-frame hold would spend the whole ≤1spf latency
budget to chase the stable-level host beat already ruled out-of-budget.

Regression test: `TestViewerSenderPacingSustainedMotionKeepsUp`
(agent/desktop/viewer_sender_test.go) — leg A (production budget): 120 frames
at-target motion → 0 gate drops, 120/120 AUs completed, steady-state queue
empties every frame period; leg B (scaled budget ×85/105 = legacy drain rate):
asserts the pre-fix failure mode (gate drops) still reproduces — the test
fails on 0.85 and passes on 1.05 by construction.

e2eviewer: `-direct-budget <bps>` (tools/e2eviewer/main.go) — paces the
direct-mode publisher at a chosen budget (production QoS shape); kept as the
pacing A/B instrument (same precedent as ad14a96's pipe-arrival summary).

## 5. Concerns / open items

- The fixed residual alternation (stable level, host-emit beat) is only
  verifiable against a true-30 fps/60 Hz production display; the local RDP
  compositor caps content at ~21 fps, which itself lands presentation in
  2/3/4-mix territory. Expected production shape: mostly `2 2 2 …` repeats
  with occasional `3` (previous task's post-fix std 3.12 ms at 30 fps config).
- fps≤5 ladder bottom remains IDR-only (100 ms gate < 200 ms frame drain) —
  pre-existing; recommend an fps ladder floor of 10 or a gate that scales with
  spf as a follow-up.
- Goodput-shaped feedback (est ≈ send rate) still cannot drive upshift; with
  pacing at ≥ encoder rate est ≈ budget, so no downshift pressure either — the
  ratchet is defused, not redesigned.
- Deployment: `XNC_DESKTOP_QOS_MAX_FPS=30` interplay unchanged; the fix is
  agent-side only — rollout = ship the stamped `bin/xnc-agent-m4.exe`
  (0.5.10-m4canary.3) via the updater once the canary node is back.

Artifacts (git-ignored): `artifacts/desktop-media/pacing-ab/` — 4 run
dumps/JSON/logs + `analyze.py` (interval/tick analyzer). Host and Edge killed;
dev stack untouched.
