# M4 Task 2 (rerun) — correctness and recovery gates on REAL console hardware

**Overall: BLOCKED — see §10 (2026-08-28 rerun-3, the current state): with the QSV hardware rung
LIVE (fb02a85), the mandated 5-minute smoke soak at native 2880x1800 fails P0 `UnrecoveredFreezes=4`
(bursty tens-of-seconds AU emission); the suite stopped per the STOP rule. The earlier §3–§8
narrative below is the PREVIOUS attempt's (software-rung) P0 and is retained as history.**

**Previous attempt summary (BLOCKED — the bounded-soak gate FAILED on a P0 counter and the suite
stopped per the STOP rule).** The run executed on labs-xiaoxin console session 5 (the designated
validation node, interactive desktop attached). The harness pipe bug from the first attempt is confirmed fixed by
commit `6f61605` (full `\\.\pipe\...` path: the rt server appeared, all viewer events dialed,
`from-diag` produced real verdicts). A second, previously-unknown harness-side defect was found and
fixed DURING this rerun (uncommitted in the worktree — see §7): `FormatStagesJson` emitted a
trailing comma in the `stages` object, so every v2 `stats.json` was invalid JSON and `from-diag`
rejected it. The binary validated here includes that fix.

Worktree: `C:\Users\LABS\Desktop\XNC\.worktrees\desktop-media-m4`, branch `codex/desktop-media-m4`,
HEAD `6f61605`. Validated binary SHA256
`4C2615CF6104B4A1DD8F4C577BD1E8E1AA9F5586C64EF1C75AE6BA48FFD5DB1B` (bin\xnc-desktop.exe, built by
`cmd /c native\desktop\build.bat` with the uncommitted pipeline.cpp fix; local v1 selftest of this
exact binary: exit 0).

---

## 1. Environment (labs-xiaoxin — all runtime evidence)

| Row | Value |
|---|---|
| Host / user | LABS-XIAOXIN, `labs-xiaoxin\labs` (WinRM user `labs`, Negotiate, empty password) |
| Session | **console session 5, ACTIVE** — proof: `query session` in-suite shows `> console LABS 5 Active` (the `>` marks the driver's own session) and `ProcessIdToSessionId` of the driver pid (7124 main suite, 20216 diag run) = 5; artifacts `logs\env-proof.txt`, `logs\env2.txt` |
| Desktop | \\.\DISPLAY11 primary, logical 1440x900 @32bpp (200% scaling); DXGI duplication native **2880x1800**; `--jpeg-single` smoke of the live desktop exit=0 (300,419 B jpg) |
| GPU | Intel(R) Arc(TM) 130T GPU (12GB), driver **32.0.101.8801** (2026-05-11), Win32_VideoController |
| OS | Microsoft Windows 11 Pro 10.0.26200 (build 26200) |
| Power | Balanced scheme; battery status=2 (on AC), 94% |
| Deploy | `C:\xnc-m4\` — repo payload incl. `bin\`, `scripts\desktop-media\`, `tools\`, `drivers\`, portable Go 1.26 at `C:\xnc-m4\go` (offline `GOPROXY=off` — run-soak builds its viewer/report tools in-run); binary hashes verified identical local↔remote (SHA256 above; accesslost.exe `B9A45018…49A4BF`, resetloop.exe `7ADD7EAB…CA759C1`) |

Execution vehicle: `Register-ScheduledTask -TaskName xnc-m4-soak` with
`New-ScheduledTaskPrincipal -UserId LABS -LogonType Interactive` and action
`powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\xnc-m4\drivers\drive-all2.ps1`
(then `diag.ps1`, `alprobe-only.ps1` for the post-abort diagnostics), `Start-ScheduledTask`,
marker-file polling via `Invoke-Command` (≥45 s intervals, per-phase wall-clock budgets). The task
was DELETED after each run; final state verified clean (no `xnc-m4*` tasks, no processes from
`C:\xnc-m4\bin`, console session 5 still Active, no desktop settings ever changed — the injector
only held a duplication).

## 2. Exact commands

Build (local worktree):
```
cmd /c native\desktop\build.bat
cd tools\e2eviewer && go build -o ..\..\bin\e2eviewer.exe .
cd tools\desktopreport && go build -o ..\..\bin\desktopreport.exe .
cd artifacts\desktop-media\resetloop-tool && GOWORK=off go build -o ..\..\..\bin\resetloop.exe .
cmd /c artifacts\desktop-media\accesslost-tool\build.bat        (ACCESS_LOST injector)
```
Deploy (WinRM, `Copy-Item -ToSession`, overwrite): `bin\{xnc-desktop,resetloop,accesslost,e2eviewer,desktopreport}.exe`,
`scripts\desktop-media\*.ps1`, driver scripts → `C:\xnc-m4\`.

On the console session (driver phases; artifacts under `C:\xnc-m4\artifacts\desktop-media\`):
```
# pilot (2 min v2; chain integrity incl. the stats.json JSON fix)
powershell -NoProfile -ExecutionPolicy Bypass -File C:\xnc-m4\scripts\desktop-media\run-soak.ps1 -DurationSec 120 -Pipeline v2 -Fps 30 -RunDir <art>\soak-pilot-20260827-151030
# mandated 60-min soak (NOT executed - suite stopped on the pilot P0):
powershell -NoProfile -ExecutionPolicy Bypass -File C:\xnc-m4\scripts\desktop-media\run-soak.ps1 -DurationSec 3600 -Pipeline v2 -Fps 30
# mandated 10-min v1 rollback soak (NOT executed):
powershell -NoProfile -ExecutionPolicy Bypass -File C:\xnc-m4\scripts\desktop-media\run-soak.ps1 -DurationSec 600 -Pipeline v1 -Fps 30
# selftests (deployed exe, console hardware)
C:\xnc-m4\bin\xnc-desktop.exe --selftest
C:\xnc-m4\bin\xnc-desktop.exe --selftest --desktop-pipeline-v2
# 100 reset cycles (NOT executed; mechanism probe only):
C:\xnc-m4\bin\resetloop.exe -exe C:\xnc-m4\bin\xnc-desktop.exe -out <art>\resetloop-100 -cycles 100 -variants auto -injector C:\xnc-m4\bin\accesslost.exe -max-total-sec 3300
```

## 3. Gate status

| Gate | Status | Evidence |
|---|---|---|
| Bounded soak (2-min pilot, mandated config): zero unrecovered freezes | **FAIL — P0** | `soak-pilot-20260827-151030\runresult.json`: `UnrecoveredFreezes=4`, Verdict FAIL. Reproduced: `soak-pilot2-20260827-151844\runresult.json`: `UnrecoveredFreezes=49`, Verdict FAIL |
| Bounded soak: zero contentId/hash regressions | **PASS (within the failed runs)** | both pilots: `OldFrameRegressions=0`, `EpochRegressions=0`; standing viewer `contentIdRegressions=0`, `rtpTsRegressions=0`, `encodeSeqRegressions=0`, `codecEpochRegressions=0` |
| Bounded soak: latency/memory gates | **FAIL** | pilot/pilot2: `CaptureToAUP95Ms` 8266/8285 (gate 15), `QueueP95Ms` 6966/6919 (gate 50), `QueueMaxMs` 7512/7468 (gate 100), `WorkingSetMB` 1190/1207 (gate 350); `CPUPercent` 2.3/2.7 |
| 60-min v2 soak | **NOT RUN — stopped on P0** (STOP rule) | suite marker `logs\suite.abort` "pilot broken: exit=1" |
| 8 h soak completion | **PENDING** (not started; blocked by the P0 above) | — |
| 100 reset cycles: one epoch per reset / recovery ≤2 s ≥99 / no stale frame | **NO EVIDENCE — 0 of 100 ran** (suite stopped on P0) | `resetloop-100` absent; mechanisms probed separately (§5) |
| 10-min v1 rollback soak (CaptureToAUP95 NO-EVIDENCE expected fail-closed) | **NOT RUN** | aborted before phase 6; expectation stands unexercised |
| Selftest v1 (`--selftest`) on console hardware | **PASS** | exit 0, 143 s, zero `SELFTEST FAIL` lines (`logs\selftest-v1.log`) |
| Selftest v2 (`--selftest --desktop-pipeline-v2`) on console hardware | **PASS** | exit 0, 179 s, zero FAIL lines; **v2e (real GDI capture of the live 2880x1800 desktop) PASSED** — the scenario that starved in the first attempt's disconnected RDP session |
| Encoder contract: fault matrix (delayed/missing/unknown-timestamp outputs; no invalid AU reaches pipe) | **PASS** (selftest both modes; checks are silent on pass — zero FAIL lines) | `logs\selftest-v{1,2}.log`; corroborating NOTEs: `mf-flush first-au-nal=6 types: 9 7 8 6 6 5`, `v2c delivery strictly monotonic n=33`, `v2k retired_drops=9`, `rsD rebuilds=6` |
| Encoder contract: 3-strike hw lock + software failover, no oscillation | **PASS** | NOTEs: `v2m hw_faults=3 creates=3 resets=3 strikes=3`, `v2n hw_creates=3 resets=2 locked=1`, `v2r hw_creates=0 backend=factory`, `v2r-rt frames=4 keys=1 rc=0` |
| Encoder contract: one epoch increment per executed reset | **PASS** | NOTEs: `v2l resets=3 storm=0/25/50`, `v2d aus=9 resets=1 rebuilds=1`, `rsA … resets=1 requests=7 merged=6`, `rsC … resets=1 w=96 h=64` |
| Lock/unlock + UAC transitions | **PENDING (manual, interactive console)** | not automatable unattended; desktop state deliberately untouched |

## 4. The P0: what failed and why (all numbers from artifacts)

Two independent 120 s soaks with the mandated config (`-Pipeline v2 -Fps 30`, no `-max-w` → native
2880x1800) failed identically:

- **Hardware encoder rung does not serve on this node.** The D3D11-aware Intel QSV MFT IS
  enumerated (`gpu_probe friendly="Intel? Quick Sync Video H.264 Encoder MFT"`, `gpu_mft_async=1
  d3d11_aware=1`), but its startup probe fails: `"probe: no METransformNeedInput within budget
  (input #1)"` (twice per run) → `media_v2_hw_contract_failure what=init strikes=1` →
  `media_v2_gpu_session_unavailable … (software rung)` → `encoder_backend=software` (stats.json),
  friendly `"CMSH264EncoderMFT (Microsoft H.264 Software Encoder)"`. The M0-ladder path likewise:
  `encoder_input_attempt backend=hardware hr=0xc00d6d77 (rejected, falling back)`. Contract
  behavior itself was correct (one loud strike, clean failover, no oscillation).
- **The software rung cannot sustain 2880x1800@30**: `mft_submit_to_output_us` p50/p95 =
  5,739,422/8,831,091 (pilot, n=320) → `capture_to_au_us` p95 8,266,477 → viewer
  `QueueP95Ms` ≈ 6.97 s. With AUs arriving in bursts seconds apart, `recoveryViolations` (post-gap
  recovery AU not starting with an IDR) accumulated: **4** in the pilot (all in the PLI event
  viewer — the requested IDR sits behind seconds of queued P-frames) and **49** in pilot2 (standing
  viewer) → `UnrecoveredFreezes` P0. Working set 1.19–1.21 GB (gate 350 MB). Secondary churn:
  `reorder_gap_skips=71 reorder_late_drops=71` (pilot). Content integrity was NOT implicated
  (all regression counters 0; `recoveryGapMs` 1164 ms with `recoveryViolations=0` on the standing
  viewer in the pilot).
- Verdict chain: `from-diag` → `verify` → `run-soak: FAIL (exit 1)`; suite aborted per the STOP
  rule (`logs\suite.abort`).

Plausibly causal environment factors (recorded, not excusing): Win11 build 26200 + Arc driver
.8801 QSV async-MFT probe timeout, 200%-scaled 2880x1800 console, Balanced power on AC. The
production shape (`--max-w` clamp / hardware rung) was NOT substituted — that would be forcing a
pass.

## 5. Reset-mechanism probes (bounded diagnostics after the STOP)

- **switch_display (0x0128)** and **set_video_config (0x0129 max_w 1280→1024)** remain the two
  scriptable unified-reset triggers against the real binary (reasons `switch` / `resolution`;
  single-display `switch_display idx=0` is accepted — `DxgiSelectDisplay` only bounds-checks the
  table). They were wired into `resetloop.exe` (`-variants`) but the 100-cycle gate never ran.
- **ACCESS_LOST by second duplication (the task's preferred mechanism) is NOT implementable on
  this OS/driver** — three probe iterations (`alprobe…`, artifacts §6):
  1. injector device-creation bug fixed (ComPtr `Reset()` nulling the kept adapter →
     `D3D11CreateDevice hr=0x80070057`);
  2. `DuplicateOutput1` with NV12 / R16G16B16A16_FLOAT / BGRA all return
     **`DXGI_ERROR_UNSUPPORTED (0x887A0004)`** while another duplication session holds the output;
  3. legacy `IDXGIOutput1::DuplicateOutput` **succeeds (hr=0x0)** and the sessions COEXIST — the
     victim kept capturing through a 300 ms hold (zero `capture_reset*` lines, epoch unchanged).
     I.e. no eviction → no ACCESS_LOST. Real-hardware ACCESS_LOST remains covered by the selftest's
     injected-backend scenario rsD (`rsD rebuilds=6` PASS).
- Desktop state was never modified (no resolution/display changes on the user's console).

## 6. Artifacts

Local copies (git-ignored) — `artifacts\desktop-media\rerun2-remote\` (remote origin
`C:\xnc-m4\artifacts\desktop-media\`):
- `soak-pilot-20260827-151030\` — runresult.json (FAIL, freezes 4), stats.json (backend=software,
  stage histograms), native.log (QSV probe chain), 5 viewer reports, metrics.jsonl, table.md
- `soak-pilot2-20260827-151844\` — confirmation FAIL (freezes 49), same shape
- `logs-final\` (= remote `logs\`): env-proof.txt, env2.txt, phase0/pilot/selftest-v1/selftest-v2/
  pilot2/env2/diag markers, suite.abort, selftest-v1.log, selftest-v2.log, pilot/pilot2 console
  logs, diag transcript, alprobe2.txt (+ `logs\`, `logs2\` same content at pull time)
- `alprobe\cycle-001\` (+`alprobe2\`,`alprobe4\`) — injector HRESULT logs (§5), cycle.json
- `prior-rerun-drive-all.ps1` — the interrupted prior rerun's driver (for provenance; its pilot
  found the stats.json JSON bug: remote `soak-20260827-144809`)

## 7. Uncommitted harness-source fix carried by this run

`native/desktop/pipeline.cpp` (worktree, NOT committed — the task mandates committing only this
doc): `FormatStagesJson` left a trailing `,\n` after the last stage member, so every v2 stats.json
was invalid JSON; `desktopreport from-diag` rejected it (found on labs-xiaoxin console session 5
by the prior interrupted rerun; the selftest's stage checks are substring matches, only a real
parse catches it). The 4-line trim fix is required for ANY v2 soak verdict to exist. Landing it as
a fix commit is a follow-up.

## 8. Follow-ups for adjudication

1. P0 driver question: QSV MFT async probe budget vs driver .8801 (probe: "no METransformNeedInput
   within budget") — hardware rung unusable on the canary node as-is.
2. Software rung at unclamped 2880x1800 is seconds-per-AU: the soak config (no `-max-w`) fails by
   construction on this node while the hardware rung is down.
3. Land the `pipeline.cpp` JSON fix (§7).
4. Reset cycles need either the two scriptable mechanisms only, or a real ACCESS_LOST trigger
   (mode change / secure-desktop) — second-duplication eviction does not exist on Win11 26200.

---

## 9. 2026-08-28 addendum — QSV hardware rung: ROOT-CAUSED AND FIXED (Outcome A)

The §4 P0 "hardware encoder rung does not serve on this node" is resolved. The
`"probe: no METransformNeedInput within budget (input #1)"` failure was misread twice: the message's
`input #1` is the probe loop's SECOND wait (0-based), and pointer-level instrumentation
(`gpu_wait_need_input` logs, `--qsv-probe-diag` mode) shows the FIRST NeedInput always arrives in
~16 ms. The stall was after the first ProcessInput, and it reproduced identically on the RDP dev
box — the earlier "understood: RDP" note was wrong; environment was never the variable.

Three independent defects in `native/desktop/mf_gpu_encoder.cpp`, all measured on driver
32.0.101.8801 on BOTH the dev box and the console (per-set bisection via a temporary
`XNC_QSV_CA_MASK` env in the diag build; artifacts `artifacts\desktop-media\qsvdiag-remote\`):

1. **`CODECAPI_AVLowLatencyMode = TRUE` wedges the MFT** — accepted (S_OK) and logged as such, but
   from the next ProcessInput on, no further METransformNeedInput is ever raised (one HaveOutput
   may still arrive; then permanent silence; drain yields nothing). `AVEncCommonLowLatency` and the
   `MF_LOW_LATENCY` attribute wedge identically. GOP size, B-picture count and submit-time
   `AVEncVideoForceKeyFrame` are innocent (verified individually).
2. **`METransformHaveOutput` is delivered ONLY to a registered `BeginGetEvent` callback** — the
   polled `GetEvent(MF_EVENT_FLAG_NO_WAIT)` queue never sees event 602. With the wedging property
   removed, a polled drive accepts 8/8 inputs yet collects ZERO outputs even across
   END_OF_STREAM+DRAIN; the callback drive receives HaveOutput ~50-75 ms after the first submit.
   Fix: `GpuEventPump` (an `IMFAsyncCallback` armed after streaming start) counts credits;
   `AbsorbCallbackCredits` folds them into the unit's counters on the media thread, which stays
   the only thread calling ProcessInput/ProcessOutput.
3. **The first ProcessOutput returns `MF_E_TRANSFORM_STREAM_CHANGE` and no fresh HaveOutput
   follows the consumed change** — the old code consumed the change inside a credit and then
   waited for a credit that never comes, stranding every AU. Fix: `PullOneOutput` renegotiates the
   MFT's offered H.264 output type (`GetOutputAvailableType`→`SetOutputType`) and retries within
   the same credit.

Remaining characterization: the encoder has a structural ~4-5-input emit depth (QSV AsyncDepth).
Every low-latency knob on this driver either wedges (defect 1) or is rejected
(LowDelayVBR / AVEncCommonRealTime → 0x80070057); a 2-frame `AVEncCommonBufferSize` is set as a
best-effort bound. First output lands at input ~5 in 11-25 ms wall — §8.3 item 2's "two inputs OR
100 ms" holds through its latency half. The selftest's `gpu-probe-first-output-strict` had
implemented the bound as AND (≤2 inputs AND ≤100 ms), contradicting both the spec wording quoted in
its own comment and the probe's own gate; corrected to the documented OR.

**Evidence (console session 5, scheduled task `xnc-m4-qsvdiag`, artifacts pulled to
`artifacts\desktop-media\qsvdiag-remote\`):**
- `--qsv-probe-diag`: exit 0 — `production_init ok=1 dt=391ms friendly="Intel? Quick Sync Video
  H.264 Encoder MFT"`; miniprobe `submitted=8 outputs=4` (AUs 7991/52/7988/8332 B).
- Full `--selftest` on console hardware: **exit 0, zero FAIL lines, hardware branch LIVE** —
  `gpu_probe ok=1 inputs=8 outputs=3 first_out_input=5 first_out_ms=25`,
  `gpu_encoder_init … async=1 provides=1 friendly="Intel? Quick Sync Video H.264 Encoder MFT"`,
  negotiated matrix 2 (BT.601) == rule, `gpu-session hardware INITIALIZED`. The hardware-branch
  CHECKs that now actually execute green: `gpu-probe-ok`, `gpu-probe-1to1-mapping`,
  `gpu-probe-first-output-strict`, `gpu-probe-discriminates`, `gpu-input-matrix-agrees` (VUI is
  not signaled by this backend — loud NOTE, matrix is the hard check).
- Same binary, dev box: local selftest exit 0.

New diagnostic surface landed with the fix (generically useful, read-only instrumentation):
`--qsv-probe-diag[-full]` mode, `XNC_QSV_DIAG=1` per-step configure logs (`gpu_cfg step=…`),
`gpu_wait_need_input` outcomes, permanent `gpu_mft_event_meerror` logging, and
`gpu_output_stream_change` traces. The probe/failover contract is unchanged — a candidate that
fails the §8.3 probe still loses the rung to software (the software failover code paths were not
modified).

§8 follow-up 1 is closed. Follow-up 2 (software rung at unclamped 2880x1800) is expected to be
overtaken by the hardware rung serving, but was NOT re-measured here — a fresh bounded soak with
the hardware rung live is the natural next gate.

---

## 10. 2026-08-28 rerun-3 — QSV hardware rung LIVE (fb02a85): SMOKE P0 → **BLOCKED**

Third execution of the Task-2 gates, after the §9 QSV fix landed (`fb02a85`, reviewed). The smoke
gate (5-minute v2 pilot soak, mandated native resolution) was executed end-to-end on labs-xiaoxin
console session 5 through the fixed harness: the rt pipe served, all five viewer events dialed,
`from-diag` produced a real verdict, and `encoder_backend=hardware` in the stats sidecar — the
hardware rung IS serving. The run **FAILED a P0 counter** (`UnrecoveredFreezes=4`). Per the STOP
rule the 60-min v2 soak, the 10-min v1 soak, the 100 reset cycles, and the console selftest rerun
were NOT executed.

### 10.1 Environment & binary identity

| Row | Value |
|---|---|
| Host / session | LABS-XIAOXIN, console session 5 ACTIVE (driver process pid 6480, `ProcessIdToSessionId`=5; `>` marker on the `console LABS 5 Active` row) — `logs\env-proof-r3.txt`, `logs\phase0.done exit=0` |
| GPU / OS | Intel Arc 130T (12GB), driver 32.0.101.8801, Win11 Pro 10.0.26200; \.\DISPLAY11 primary 1440x900@32bpp logical (200% scaling) → DXGI duplication native 2880x1800; `--jpeg-single` smoke of the live desktop exit=0 |
| Binary | `xnc-desktop.exe` SHA256 `073E59649E8A437B29BB3D6EAA96E5EC2B119093FFA507DFFEA908DEB2F1924D`, built from clean `fb02a85` (`cmd /c native\desktop\build.bat`), hash-verified local↔remote at deploy (also e2eviewer `FA64AB4C…B1B4EF`, desktopreport `46FEDED9…EBB38`, resetloop `7ADD7EAB…CA759C1` unchanged from the prior rerun) |
| Vehicle | scheduled task `xnc-m4-t2r3` (`LABS` Interactive principal) running `C:\xnc-m4\drivers\drive-rerun3.ps1 -Phase <name>`; one phase per start; WinRM marker polling (≥60 s intervals); per-phase wall budgets |

Cleanup verified after the STOP: `xnc-m4*` tasks remaining = 0, no processes from `C:\xnc-m4\bin`,
console session 5 still Active, no display/resolution/desktop setting ever changed (no reset
trigger was fired in this run).

### 10.2 Exact commands

```
# local build + deploy (hash proof in deploy console transcript)
cmd /c native\desktop\build.bat
cd tools\e2eviewer && go build -o ..\..\bin\e2eviewer.exe .
cd tools\desktopreport && go build -o ..\..\bin\desktopreport.exe .
cd artifacts\desktop-media\resetloop-tool && GOWORK=off go build -o ..\..\..\bin\resetloop.exe .
powershell -File artifacts\desktop-media\deploy-rerun3.ps1 -RegisterTask

# remote, console session 5 (task xnc-m4-t2r3, phase per start)
#   -Phase phase0   environment proof                     -> phase0.done exit=0
#   -Phase smoke -SmokeSec 300  (5-min v2 pilot soak)     -> smoke.done exit=1  (P0; suite STOPPED)
powershell -File artifacts\desktop-media\run-phase3.ps1 -Phase smoke -ExtraArgs "-SmokeSec 300"
#   the harness command the smoke phase ran on the console:
powershell -NoProfile -ExecutionPolicy Bypass -File C:\xnc-m4\scripts\desktop-media\run-soak.ps1 -DurationSec 300 -Pipeline v2 -Fps 30 -RunDir C:\xnc-m4\artifacts\desktop-media\soak-smoke-20260827-211341
# NOT executed (STOP rule): -Phase soakv2 (3600s), -Phase soakv1 (600s), -Phase cycles (100), -Phase selftestv2
```

### 10.3 The P0 (all numbers from the artifacts)

`runresult.json` (`Verdict: FAIL`):
`UnrecoveredFreezes=4` (P0), `OldFrameRegressions=0`, `EpochRegressions=0`,
`CaptureToAUP95Ms=59138.683` (gate 15), `QueueP95Ms=18270.4` (gate 50), `QueueMaxMs=60001.5`
(gate 100), `WorkingSetMB=432.9` (gate 350), `CPUPercent=0.221`, Resolution 2880x1800@30fps,
DurationSec 300.

`stats.json` (valid JSON, `ok=1`): **`encoder_backend=hardware`** (the §9 fix holds in production
shape; `cpu_readbacks=0`), `captured=30`, `encoded=158` (`warmup_feeds=128`), `keyframes=65`,
`timeouts=8846`, `resets=0`, `rebuilds=0`, `aus_written=157`. Terminal native line:
`media_v2_stop elapsed=300546ms captured=30 encoded=158 keyframes=65 timeouts=8846 warmup_feeds=128
resets=0 rebuilds=0 w=2880 h=1800 aus=157 bytes=2596824 reorder_gap_skips=0 reorder_late_drops=0
backend=hardware cpu_readbacks=0 ok=1`.

Stage histograms (the measured **paced capture→AU** numbers — this run's 30fps-paced feed through
the LIVE hardware rung at native 2880x1800; n=157 AUs):

| stage | p50 | p95 | p99 |
|---|---|---|---|
| `gpu_copy_us` | 367 | 26,832 | 26,867 |
| `gpu_convert_us` | 454 | 789 | 3,301 |
| `mft_submit_to_output_us` | 118,807 | 4,972,288 | 36,517,211 |
| `queue_age_us` | 40,602,983 | 58,100,366 | 59,632,510 |
| `capture_to_au_us` | **41,652,505** | **59,138,683** | **60,040,206** |

Viewer-side shape (standing viewer `viewer-01-viewer.json`): 21 AUs in the first 1.2 s
(155→1159 ms), then emission gaps of ~21 s, ~36 s, ~19 s followed by dense catch-up bursts
(~100 ms cadence) — a bursty/stalled emit pattern, not steady pacing. The PLI viewer
(`viewer-02-pli.json`) shows `firstFrameMs=5104` (>2000 ms assertion), `queueAgeP95Ms=18270`,
`pliToIdrMaxMs=52`, `recoveryGapMs=678`, `recoveryViolations=0`; the 4 freezes are the burst-gap
recoveries whose first post-gap AU is not an IDR.

**Task-3 known-gap data point (measured, not assumed):** the predicted ~130–165 ms paced
capture→AU latency (from the 4–5-input QSV emit depth) is falsified at native 2880x1800 on driver
32.0.101.8801 — measured p50 ≈ 41.7 s / p95 ≈ 59.1 s / p99 ≈ 60.0 s under the 30fps paced feed,
i.e. the emit pipeline dwells tens of seconds between bursts even though CPU is 0.2% and
`resets/rebuilds/strikes` are all zero. These are failure-stage numbers from the P0 run, recorded
as the current hardware-rung latency reality; they do NOT satisfy the 15 ms gate and were not
re-measured at other resolutions (the STOP rule halts iteration).

Content integrity was NOT implicated (`OldFrameRegressions=0`, `EpochRegressions=0`,
`rtpTsRegressions/contentIdRegressions/encodeSeqRegressions/codecEpochRegressions=0` in every
viewer report) — this is again a latency/freeze failure, with the hardware rung serving.

### 10.4 Dev-box finding during pre-deploy sanity (recorded, not iterated)

The same clean-`fb02a85` build SEGFAULTS in the local dev-box (LABS-DEV, disconnected-RDP session
1, Arc 140T driver 32.0.101.7026) v1 selftest, deterministically (2 of 2 runs), immediately after
`gpu_probe friendly="Intel? Quick Sync Video H.264 Encoder MFT" ok=1 inputs=8 outputs=3
first_out_input=4|5 first_out_ms=9` — i.e. right after the hardware startup probe succeeds, inside
the gpu-session hardware scenario (`desktop_selftest.cpp` ~L2986 `run_probe(gpu)` region:
submit/TakeOutput loop or the MF decoder hash). Bash reports `Segmentation fault` (exit 139);
stdout is lost to block buffering (0-byte stdout artifact). Artifacts:
`artifacts\desktop-media\selftest-local-rerun3-v1.log` (58 lines, terminal `exit=127`),
`selftest-local-rerun3-v1b-stdout.log` (0 bytes), `selftest-local-rerun3-v1b-stderr.log` (57
lines, ends at the `gpu_probe ok=1` line). This crash was NOT reproduced inside this task's
executed scope on labs-xiaoxin (the console selftest phase was skipped by the STOP rule; the
console smoke ran the production pipeline for the full 300 s without crashing) — it is flagged for
adjudication as a separate QSV-adjacent defect signal, not evidence about the validation node.

### 10.5 Gate status after rerun-3

| Gate | Status | Evidence |
|---|---|---|
| Smoke (5-min v2, native 2880x1800, hardware rung): zero unrecovered freezes | **FAIL — P0** | §10.3; `soak-smoke-20260827-211341\runresult.json`: `UnrecoveredFreezes=4`, Verdict FAIL |
| Smoke: hardware backend confirmed serving | **PASS** (within the failed run) | `stats.json` `encoder_backend=hardware`, `cpu_readbacks=0` |
| Smoke: zero contentId/hash regressions | **PASS** (within the failed run) | `OldFrameRegressions=0`, `EpochRegressions=0`; all viewer regression counters 0 |
| Smoke: latency/memory gates | **FAIL** | `CaptureToAUP95Ms` 59,138.7 (gate 15), `QueueP95Ms` 18,270.4 (gate 50), `QueueMaxMs` 60,001.5 (gate 100), `WorkingSetMB` 432.9 (gate 350) |
| Paced capture→AU percentiles (Task-3 known-gap data point) | **MEASURED — fails the 15 ms gate by ~4000x** | §10.3 stage table (p95 ≈ 59.1 s) |
| 60-min v2 soak | **NOT RUN — stopped on smoke P0** (STOP rule) | suite stopped after `smoke.done exit=1` |
| 8 h soak completion | **PENDING** (not started; command per ruling 1: `run-soak.ps1 -DurationSec 28800 -Pipeline v2 -Fps 30`) | — |
| 10-min v1 rollback soak (CaptureToAUP95 NO-EVIDENCE expected) | **NOT RUN — stopped on smoke P0** | expectation unexercised |
| 100 reset cycles: one epoch per reset / recovery ≤2 s ≥99 / no stale frame | **NO EVIDENCE — 0 of 100 ran** (STOP rule) | mechanisms unchanged from §5 (switch_display 0x0128 / set_video_config 0x0129) |
| Selftest v1/v2 on console, current build | **NOT RUN on console — stopped on smoke P0**; dev-box v1 selftest CRASHES post-probe (§10.4) | prior-attempt console selftests (§3) ran the earlier build |
| Lock/unlock + UAC transitions | **PENDING (manual, interactive console)** | not automatable unattended |

### 10.6 Artifacts (git-ignored)

Local: `artifacts\desktop-media\rerun3-remote\`
- `soak-smoke-20260827-211341\` — runresult.json (FAIL, freezes 4), stats.json (backend=hardware,
  stage histograms), native.log (645 lines, terminal `media_v2_stop … backend=hardware`), 5 viewer
  reports + stderr logs, metrics.jsonl, table.md
- `logs\` — env-proof-r3.txt, phase0.done, smoke.txt, smoke.done, smoke-console.log,
  drive-rerun3-{phase0,smoke}-*.log transcripts (plus the prior attempts' logs pulled earlier)
- Dev-box segfault: `artifacts\desktop-media\selftest-local-rerun3-v1{,b-stdout,b-stderr}.log`
- Tooling (artifacts-local): `drive-rerun3.ps1`, `deploy-rerun3.ps1`, `run-phase3.ps1`,
  `cleanup-rerun3.ps1`

### 10.7 Concerns for adjudication

1. The hardware rung now initializes and produces (the probe fix holds in production shape), but
   AU emission at native 2880x1800 is bursty with tens-of-seconds dwell
   (`mft_submit_to_output_us` p99 ≈ 36.5 s; `capture_to_au_us` p95 ≈ 59.1 s) while CPU sits at
   0.2% and resets/rebuilds/strikes are zero — the emit path (event pump / credit accounting /
   output drain at 5.2 MP) needs investigation before any soak can pass. The 15 ms Task-3 gate is
   nowhere near met by the measured numbers.
2. The dev-box selftest SEGFAULT immediately after a successful QSV startup probe (§10.4) is a
   second, independent QSV-adjacent defect signal in `fb02a85`; a crash dump / repro on an
   attached session should triage it.
3. All recovery gates (100 cycles, v1 rollback soak, console selftest on the current build) remain
   unexercised behind the smoke P0 — no pass/fail claim exists for them; the 8 h soak and lock/UAC
   remain PENDING as ruled.
