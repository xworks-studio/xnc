# M4 Task 2 (rerun) — correctness and recovery gates on REAL console hardware

**Overall (2026-08-28 rerun-4, §12 — the current state): the mandated gates EXECUTED CLEAN of P0 on
the repaired hardware path (0001b94): smoke 5-min v2 and 60-min v2 soak both `UnrecoveredFreezes=0`
with all content regressions 0 and `encoder_backend=hardware`; 100/100 reset cycles pass all three
recovery gates; console v2 selftest exit 0 with the new pump-stress/idle-flush pins green on real
hardware. The latency/memory gates (CaptureToAUP95 / QueueP95 / WorkingSet) still FAIL — non-P0,
recorded as the Task-3 known-gap data. 8 h soak and lock/UAC transitions remain PENDING (exact
command / manual). §3–§11 are retained history (previous attempts' P0s and the QSV repair).**

**Previous attempt summary (BLOCKED — the bounded-soak gate FAILED on a P0 counter and the suite
stopped per the STOP rule).** The run executed on labs-xiaoxin console session 5 (the designated
validation node, interactive desktop attached). The harness pipe bug from the first attempt is confirmed fixed by
commit `6f61605` (full `\\.\pipe\...` path: the rt server appeared, all viewer events dialed,
`from-diag` produced real verdicts). A second, previously-unknown harness-side defect was found and
fixed DURING this rerun (uncommitted in the worktree — see §7): `FormatStagesJson` emitted a
trailing comma in the `stages` object, so every v2 `stats.json` was invalid JSON and `from-diag`
rejected it. The binary validated here includes that fix.

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

## 11. 2026-08-28 follow-up — segfault root-caused + burst/stall CHARACTERIZED (content-regime, not resolution); idle-flush landed

Two commits on top of `fb02a85`: `fix(desktop): harden gpu event pump teardown` (the §10.4 dev-box
segfault) and the v2 idle-flush (below). All labs-xiaoxin evidence: console session 5 via the
scheduled-task pattern, artifacts under `C:\xnc-m4\artifacts\desktop-media\qsvfix\` + `logs\`
(markers `matrix/static/smoke*/.done`), driver `artifacts\desktop-media\drive-qsvfix.ps1`.

### 11.1 Defect 1 (dev-box selftest segfault) — FIXED

Reproduced locally 1-in-4 plain `--selftest` runs: log terminates exactly at
`gpu_probe ok=1`, i.e. inside `RunStartupProbe`'s `ReleaseUnit(probe)`. Root cause as the review
suspected: `GpuEventPump::Stop()` was a bare atomic store, so it could land between `Invoke`'s
stopped-check and the re-arm `gen_->BeginGetEvent`, arming a callback that then called
`EndGetEvent`/`BeginGetEvent` on a generator whose driver session `ShutdownObject` was
concurrently tearing down. Fix: a `CRITICAL_SECTION` barrier — `Invoke` performs ALL generator
calls under the lock and never touches the generator once `stopping_` is observed; `Stop()` sets
the flag under the same lock, so when it returns no `Invoke` is in-flight inside the generator
(and none will re-enter) before `END_STREAMING`/`ShutdownObject` run; the pump's own reference is
released after those. A second lifetime hole found while hardening: `ConfigureUnit` previously
dropped its own pump reference after arming, so MF completing the pending op without a re-arm
(`Invoke`'s EndGetEvent-failure path) self-destructed the pump while `Unit::pump` still pointed
at it (intermittent v2-selftest teardown crash, ~1-in-2 with the flush active) — the Unit now
keeps its own reference. Verification: plain selftest ×11 + v2 ×4 exit 0 across the two builds
(0 crashes; pre-fix rate 1-in-4 plain).

### 11.2 Defect 2 (QSV burst/stall at native) — CHARACTERIZED + mitigated

Per-hypothesis measurements, all 60 s `--console-diag --desktop-pipeline-v2 --fps 30` (or the
5-min run-soak smoke harness = the RERUN-2 P0 workload), driver 32.0.101.8801, backend=hardware
in every run, probe always the gate:

**(a) Resolution dependence — DISPROVED.** With continuous desktop content (~21 changes/s — the
console had a playing video + FPS-test page):

| run (--max-w) | dims | mft dwell p50/p95/p99 (ms) | capture→AU p50/p95/p99 (ms) | AUs |
|---|---|---|---|---|
| 1920 | 1920x1200 | 65 / 68 / 69 | 110 / 113 / 115 | 1267 |
| 1600 | 1600x1000 | 65 / 68 / 69 | 110 / 113 / 115 | 1269 |
| 1280 | 1280x800 | 65 / 68 / 70 | 110 / 113 / 115 | 1268 |
| 1024 | 1024x640 | 65 / 68 / 70 | 110 / 113 / 115 | 1269 |
| native | **2880x1800** | **65 / 68 / 70** | **110 / 113 / 115** | 1265 |

There is NO resolution boundary — 5.2 MP paces identically to 1024 px wide. The RERUN-2
"resolution" reading was a confound: that run's desktop was (nearly) static (captured=30/300 s).

**(b) AVEncCommonBufferSize unit (review Minor #1) — measured INERT, default unchanged.** The
RERUN-2 P0 workload re-run via the exact soak harness: legacy bytes-ish value → 4 unrecovered
freezes, capture→AU p95 59.3 s (REPRODUCES RERUN-2 verbatim). With the documented bits unit
(`XNC_QSV_BUFSZ=bits`, two-frames-in-bits) under a matched content regime (~2 changes/s,
captured 589 vs 587): freezes 0 vs 0, p95 605.6 vs 606.2 ms, mft p99 606 vs 606 ms — IDENTICAL.
The driver ignores the property at both magnitudes; the knob stays (XNC_QSV_BUFSZ=bits|off) for
other drivers, the legacy default stays (land-only-what-helps).

**(c) Explicit CBR + MeanBitRate (flush isolated off) — measured INERT.** `XNC_QSV_RC=cbr` +
bits buffer under the matched regime (captured 569): mft dwell p50/p95/p99 587/602/606 ms, 0
freezes, capture→AU p95 604 ms — identical to legacy (586/605/606) and to bits-only (586/605/606);
legacy already falls back to CBR mode after the LowDelayVBR 0x80070057 rejection. The entire
codecapi property family (buffer magnitude, explicit RC, MeanBitRate) is inert on driver
32.0.101.8801; only input cadence and the flush move the numbers.

**Actual mechanism (from the stage decomposition).** The stage named `mft_submit_to_output_us`
stamps at SUBMISSION and is observed at collection: it is a real encoder dwell. On a static
desktop the rungs park the tail of a content burst inside their emit depth (QSV ~5 inputs — the
fb02a85 structural finding; the software MFT ~17) and emit only when further inputs arrive; the
pipeline's IdleFeed only re-fed during warmup or while an IDR was in flight (PLI-driven), so
between desktop changes the stream froze for the whole gap — tens of seconds — then flushed as a
catch-up burst. `queue_age_us`' tens-of-seconds p50 is the same-content re-feed stamps (feeds
re-publish content whose source stamp is the original capture), a measurement artifact of the
feed path, not a mailbox delay.

**Mitigation landed (v2 pipeline): idle park flush, default ON.** When outputs are owed
(submissions not yet emitted), input has starved >2 slots, and no warmup/IDR feed is active,
re-feed the same surface (the spec §7.4/§7.5 feed mechanism extended), ≤8 feeds per content
episode, episode reset on the next captured frame. `XNC_QSV_IDLE_FLUSH=0` disables. Measured
(same 5-min smoke workload, native, legacy tuning): mft dwell p50/p95 586/605 ms → **114/119 ms**,
0 unrecovered freezes, 0 content/epoch regressions, `resets=rebuilds=0`, backend=hardware. The
remaining ~0.6 s capture→AU p95 in that run is feed-stamp artifacts (feeds carry the original
content stamp; real-frame latency ≈ mft dwell ≈ 119 ms p95 + queue 78 ms). The fully-static P0
regime is mechanism-bounded to ~trigger(66 ms)+8×spf+encode (<1 s) but was not re-measured in a
verifiably fully-static console state (could not control desktop staticity remotely).

### 11.3 Follow-up note (replaces the §10.7 max-w pinning idea)

The hardware rung does NOT need a max-w pin for latency — native 2880x1800 paces at 65-70 ms
under motion. The characterization for adjudication: burst/stall = static-content input
starvation parking the encoder's emit tail, mitigated by the idle flush; the 15 ms Task-3 gate
remains unmet at low content cadence (p95 ~119 ms dwell is the QSV emit depth at feed cadence —
deeper driver-level low-latency knobs remain wedging/rejected on 32.0101.8801, per fb02a85).

### 11.4 Artifacts

Local: `artifacts\desktop-media\qsvfix-local-desktop-now.jpg` (the console's content regime during
the matrix), markers mirrored in this section. Remote (`\labs-xiaoxin\C$\xnc-m4\artifacts\
desktop-media\`): `qsvfix\<run>\{out.h264,stats.json,console.log}`, `logs\{matrix,static,smoke,
smokebits,smokeflush,smokecbr}.done` + `drive-qsvfix-*.log` transcripts + soak run dirs.

---

## 12. 2026-08-28 rerun-4 — repaired hardware path (0001b94): ALL GATES EXECUTED, P0 CLEAN → suite completes

Fourth execution of the Task-2 gates, after the §9 probe fix, the §11 segfault/burst fixes, and the
regression pins landed (`0001b94` "test(desktop): pin gpu pump races and idle flush reset"). Every
phase of the execution plan ran end-to-end on labs-xiaoxin console session 5; **no P0 counter fired
anywhere in this suite** — the STOP rule was never triggered. No code was changed; the only commit
created is this results doc.

### 12.1 Environment & binary identity

| Row | Value |
|---|---|
| Host / session | LABS-XIAOXIN, console session 5 ACTIVE (phase-0 driver pid 27684, `ProcessIdToSessionId`=5, `>` marker on the `console LABS 5 Active` row) — `logs\env-proof-r3.txt` (refreshed this run), `logs\phase0.done exit=0` |
| GPU / OS | Intel Arc 130T (12GB), driver 32.0.101.8801 (2026-05-11), Win11 Pro 10.0.26200; \.\DISPLAY11 primary 1440x900@32bpp (200% scaling) → DXGI native 2880x1800; `--jpeg-single` live-desktop smoke exit=0 (269,562 B) |
| Binary | `xnc-desktop.exe` SHA256 `9C7E966C29CB86848F0CCD8A1E3660DC5F5A77BEFBCF32BAEC25CD568E2AED95`, built from clean `0001b94` (`cmd /c native\desktop\build.bat`), hash-verified local↔remote at deploy (e2eviewer `FA64AB4C…B1B4EF`, desktopreport `46FEDED9…EBB38`, resetloop `7ADD7EAB…CA759C1` — all three unchanged from prior reruns) |
| Vehicle | scheduled task `xnc-m4-t2r4` (`LABS` Interactive principal) → `C:\xnc-m4\drivers\drive-rerun4.ps1 -Phase <name>`; one phase per start; WinRM marker polling (≥60 s); per-phase wall budgets respected (smoke 5:10, soak 60:15, v1 10:15, cycles 16:55, selftest 3:20) |

Cleanup verified after the suite: `xnc-m4*` tasks remaining = 0, no processes from `C:\xnc-m4\bin`,
console session 5 still Active, no display/resolution/desktop setting changed (the reso_rt cycles
change the native's internal max-w only; switch_display idx=0 re-selects the same single display).

### 12.2 Exact commands

```
# local build + deploy (hash proof in deploy transcript)
cmd /c native\desktop\build.bat
cd tools\e2eviewer && go build -o ..\..\bin\e2eviewer.exe .
cd tools\desktopreport && go build -o ..\..\bin\desktopreport.exe .
cd artifacts\desktop-media\resetloop-tool && GOWORK=off go build -o ..\..\..\bin\resetloop.exe .
powershell -File artifacts\desktop-media\deploy-rerun4.ps1

# remote, console session 5 (task xnc-m4-t2r4, one phase per start)
powershell -File artifacts\desktop-media\run-phase4.ps1 -Phase phase0
powershell -File artifacts\desktop-media\run-phase4.ps1 -Phase smoke    -ExtraArgs "-SmokeSec 300"
powershell -File artifacts\desktop-media\run-phase4.ps1 -Phase soakv2   # 3600 s default
powershell -File artifacts\desktop-media\run-phase4.ps1 -Phase soakv1   # 600 s default
powershell -File artifacts\desktop-media\run-phase4.ps1 -Phase cycles   # 100, budget 3300 s
powershell -File artifacts\desktop-media\run-phase4.ps1 -Phase selftestv2
# the harness commands the phases ran (C:\xnc-m4\scripts\desktop-media\run-soak.ps1):
#   smoke: -DurationSec 300  -Pipeline v2 -Fps 30 -RunDir <art>\soak-smoke-20260828-004709
#   soak : -DurationSec 3600 -Pipeline v2 -Fps 30        -> soak-20260828-005556
#   v1   : -DurationSec 600  -Pipeline v1 -Fps 30        -> soak-20260828-015941
# cycles: resetloop.exe -exe xnc-desktop.exe -out resetloop-100-r4 -cycles 100 -variants switch_diag,reso_rt -max-total-sec 3300
```

### 12.3 Smoke (5-min v2 pilot, native 2880x1800) — P0 GATE PASSED

`runresult.json` (`Verdict: FAIL` — latency/memory gates only, §12.6):
`UnrecoveredFreezes=0`, `OldFrameRegressions=0`, `EpochRegressions=0` (all P0 counters zero),
`CaptureToAUP95Ms=241.215` (gate 15), `QueueP95Ms=64.81` (gate 50), `QueueMaxMs=90.95` (gate 100
PASS), `WorkingSetMB=413.6` (gate 350), `CPUPercent=0.329`.

`stats.json` (`ok=1`): **`encoder_backend=hardware`**, `cpu_readbacks=0`, captured=5828,
encoded=6159, keyframes=80, `resets=0`, `rebuilds=0`, 2880x1800. Suite proceeded past the gate
(P0-zero + hardware backend are the smoke gate; the latency-gate FAIL is the pre-declared Task-3
known-gap, not a P0).

### 12.4 60-min v2 soak (native, default events) — P0 GATE PASSED

`runresult.json`: `UnrecoveredFreezes=0`, `OldFrameRegressions=0`, `EpochRegressions=0`;
`CaptureToAUP95Ms=593.568` (gate 15 FAIL), `QueueP95Ms=72.52` (gate 50 FAIL), `QueueMaxMs=101.92`
(gate 100 FAIL — 101.92 ms > 100 ms, the gate is exclusive), `WorkingSetMB=413.9` (gate 350 FAIL),
`CPUPercent=0.221`, `Verdict: FAIL` (latency/memory only).

`stats.json`: **`encoder_backend=hardware`**, captured=70845, encoded=74359, keyframes=306,
`resets=0`, `rebuilds=0`, `cpu_readbacks=0`; terminal native line
`media_v2_stop elapsed=3600515ms … aus=74358 bytes=660092903 reorder_gap_skips=0
reorder_late_drops=0 backend=hardware ok=1`; `desktop_watch_stop polls=7037 failures=0
state=Default`. **625 `idle_flush_begin` events** — the §11.2 idle park flush actively served the
static stretches; zero freezes resulted (the rerun-3 failure shape is gone).

Viewer-side shape (all five events, zero regression counters everywhere):
- standing viewer: **74,331 frames** delivered, `firstFrameMs=183`, `queueAgeP95Ms=72.5`,
  `recoveryGapMs=354`, `recoveryViolations=0`;
- PLI viewer: `firstFrameMs=198`, `pliToIdrMaxMs=152`, `queueAgeP95Ms=29.1`, `recoveryViolations=0`;
- burst/reconnect viewers: same clean shape (files in the run dir).

### 12.5 10-min v1 rollback soak — documented NO-EVIDENCE row

`runresult.json`: `Verdict: NO-EVIDENCE` with `CaptureToAUP95Ms=0` — the documented fail-closed
expectation (the v1/M0 pipeline has no stage instrumentation, so its latency percentile is
NO-EVIDENCE by design). Recorded counters (the rollback row, not a v2 gate):
`UnrecoveredFreezes=363`, `OldFrameRegressions=1`, `QueueP95Ms=7719.2`, `WorkingSetMB=1103.9`,
`CPUPercent=15.7`; stats `captured=2956 encoded=3711 keyframes=169 resets=0` (no `stages` object,
no `encoder_backend` field — v1 sidecar shape). This is the known v1 degradation the v2 pipeline
(idle flush + hardware rung) exists to fix — retained as the rollback-path evidence.

### 12.6 Paced capture→AU percentiles (Task-3 known-gap data, measured)

Stage histograms from the two v2 runs (n = 4096-sample window; hardware rung, native 2880x1800,
30 fps paced feed, driver 32.0.101.8801):

| run | stage | p50 | p95 | p99 |
|---|---|---|---|---|
| smoke 300 s | `capture_to_au_us` | 109,803 | 241,215 | 565,357 |
| smoke 300 s | `mft_submit_to_output_us` | 64,961 | 102,450 | 117,817 |
| soak 3600 s | `capture_to_au_us` | 196,576 | 593,568 | 630,587 |
| soak 3600 s | `mft_submit_to_output_us` | 83,906 | 118,684 | 121,077 |
| both | `gpu_copy_us` | ~318 | 698–14,184 | 6,789–30,415 |
| both | `gpu_convert_us` | ~500–640 | ~1,000–1,240 | ~1,900–2,800 |

**Task-3 known-gap data (measured, not assumed):** hardware-rung paced capture→AU at native sits at
p50 ≈ 110–197 ms / p95 ≈ 241–594 ms / p99 ≈ 0.57–0.63 s across the two regimes (motion-heavy smoke
vs the 60-min mixed/static soak; the soak tail includes the idle-flush feed-stamp artifacts
documented in §11.2). The 15 ms Task-3 gate remains unmet by ~16–40x — this is the number Task-3
planning must use. Viewer-experienced queueing stayed small (`queueAgeP95Ms` 72.5 standing / 29.1
PLI) and recovery stayed IDR-first — the latency failure is in the emit-depth dwell, not queueing.

### 12.7 100 reset cycles — ALL THREE GATES PASS (100/100)

`resetloop-100-r4` (50 `switch_diag` 0x0128 + 50 `reso_rt` 0x0129, 09:15:47–09:32:37Z, within the
3300 s budget; ACCESS_LOST injection remains impossible on Win11 26200 — §5). Recounted directly
from `cycles.jsonl` (100 lines):

| Gate | Result | Numbers |
|---|---|---|
| epoch increment EXACTLY 1 per reset | **100/100 PASS** | zero failures |
| recovery ≤ 2 s (target ≥ 99/100) | **100/100 PASS** | p50 1,309 ms / p95 1,504 ms / max 1,677 ms; switch_diag p50 1,431 / p95 1,523 / max 1,677; reso_rt p50 1,130 / p95 1,250 / max 1,317 |
| stale frames = 0 | **100/100 PASS** | zero failures |

Host corroboration: `capture_reset_done` exactly once per cycle in all 100 native logs (reasons
`switch`/`resolution`, reset elapsed ≈ 78 ms), `hw_contract_failures=0` everywhere, and
`first_new_epoch_frame_is_key=100/100` (every post-reset stream resumes IDR-first).

Tool-harness caveats (documented, not product failures): the tool's own `cycles_all_gates_ok=50`
and the 50 `failing_cycles` entries for switch_diag are caused solely by its auxiliary
`stats.json` check — the runner kills the console-diag native at the end of its 5 s watch tail,
before the 15 s `--duration` would flush `stats.json` (`stats_err="stats.json missing"`; reso_rt
cycles are exempt by design because rt mode writes no sidecar). `summary.json`'s top-level
`cycles: 0` is a second tool artifact (the counter is never incremented in main.go). The three
mandated gates above are computed per-cycle from the v2 wire + native logs and are unaffected.

### 12.8 Console hardware v2 selftest (deployed build, session 5) — PASS

`--selftest --desktop-pipeline-v2` on the deployed `0001b94` binary: **exit 0, 2,753 log lines,
zero `SELFTEST FAIL` lines**, terminal `selftest ok`. Citations (verbatim NOTE lines):

- **New pins, real hardware:** `gpu-pump-stress iters=12 ok=12 dt=6719ms (pre-fix 729bfaf: 8
  crashes/20 qsvdiag runs; post-fix 0/20)`; `v2s idle-flush subs=6->14 aus=1->9 (delay=5 park;
  flush env=on)`; `v2t flush-reset subs=14(pre)15(rebuild)23(post) rebuilds=1 resets=1
  flush_env=on` — the exact UAF-window stress and both idle-flush pins pass on the Arc 130T.
- **Hardware branch:** repeated `gpu_probe friendly="Intel? Quick Sync Video H.264 Encoder MFT"
  ok=1` and `gpu_encoder_init ... async=1` (the stress loop re-inits 12x without a crash).
- **v2e (real GDI capture of the live 2880x1800 desktop) now serves on the HARDWARE rung:**
  `v2e rung=hardware friendly="Intel? Quick Sync Video H.264 Encoder MFT" w=2880 h=1800 aus=10
  keys=1 feeds=2` (in §3's attempt it starved; in §10's build it was software-rung at best).
- **Encoder contracts (silent-pass CHECKs + NOTEs):** `mf-flush first-au-nal=6 types: 9 7 8 6 6
  5`; `rsA ... resets=1 requests=7 merged=6`; `rsC ... resets=1 w=96 h=64`; `rsD rebuilds=6`;
  `v2d aus=9 resets=1 rebuilds=1`; `v2c rung=hardware ... n=55 ... strictly monotonic`;
  `v2k retired_drops=9 aus=19 gap_skips=10`; `v2l resets=3 storm=0/25/50`;
  `v2m hw_faults=3 creates=3 resets=3 strikes=3`; `v2n hw_creates=3 resets=2 locked=1`;
  `v2r hw_creates=0 backend=factory aus=2`.

### 12.9 Gate status after rerun-4 (the current state)

| Gate | Status | Evidence |
|---|---|---|
| Smoke (5-min v2, native, hardware rung): zero P0 counters | **PASS** | §12.3; freezes/regressions all 0 |
| Smoke: `encoder_backend=hardware` | **PASS** | §12.3 stats.json |
| Smoke: latency/memory gates | **FAIL** (non-P0, known-gap) | CaptureToAUP95 241.2 ms (gate 15), QueueP95 64.8 (gate 50), WS 413.6 (gate 350) |
| 60-min v2 soak: zero unrecovered freezes | **PASS** | §12.4 `UnrecoveredFreezes=0` |
| 60-min v2 soak: zero contentId/hash/epoch regressions | **PASS** | §12.4 all viewer + runresult counters 0 |
| 60-min v2 soak: latency/memory gates | **FAIL** (non-P0, known-gap) | CaptureToAUP95 593.6, QueueP95 72.5, QueueMax 101.9 (FAIL — 101.9 ms > 100 ms exclusive), WS 413.9 |
| Paced capture→AU percentiles (Task-3 known-gap data) | **MEASURED** — p95 241–594 ms, fails the 15 ms gate ~16–40x | §12.6 table |
| 10-min v1 rollback soak | **DOCUMENTED** — `Verdict: NO-EVIDENCE` on CaptureToAUP95 as expected (fail-closed); rollback counters recorded (freezes 363, 1 regression, WS 1104) | §12.5 |
| 100 cycles: epoch increment exactly 1 | **PASS 100/100** | §12.7 |
| 100 cycles: recovery ≤ 2 s for ≥ 99 | **PASS 100/100** (p95 1,504 ms) | §12.7 |
| 100 cycles: no stale frame | **PASS 100/100** | §12.7 |
| ACCESS_LOST injection cycles | **NOT POSSIBLE on Win11 26200** (standing finding §5); covered by selftest rsD (injected backend, PASS §12.8) | §5, §12.8 |
| Console v2 selftest (current build) | **PASS** (exit 0, 0 FAIL, new pins green on hardware) | §12.8 |
| 8 h soak completion | **PENDING** — exact command: `powershell -NoProfile -ExecutionPolicy Bypass -File C:\xnc-m4\scripts\desktop-media\run-soak.ps1 -DurationSec 28800 -Pipeline v2 -Fps 30` (from the repo: `run-soak.ps1 -DurationSec 28800 -Pipeline v2 -Fps 30`); not started in this suite | — |
| Lock/unlock + UAC transitions | **PENDING** (manual, interactive console; not automatable unattended; manual procedure: trigger Win+L lock and a UAC elevation prompt on the console, expect capture_reset_start/done with reason desktop_switch/access_lost in the native log + epoch advance + viewer recovery — the cycle driver's observation channels) | — |

### 12.10 Artifacts (git-ignored)

Local: `artifacts\desktop-media\rerun4-remote\` (~807 MB; remote origin
`C:\xnc-m4\artifacts\desktop-media\`):
- `soak-smoke-20260828-004709\`, `soak-20260828-005556\`, `soak-20260828-015941\` — each: runresult,
  stats, native.log, 5 viewer reports + stderr, metrics.jsonl, table.md, capture.h264
- `resetloop-100-r4\` — summary.json, cycles.jsonl, cycle-001..100 (native.log + cycle.json each)
- `logs\` — env-proof-r3.txt, phase0.done, smoke/soakv2/soakv1/cycles/selftestv2 markers + console
  logs, selftest-v2-r4.log, drive-rerun4-<phase>-*.log transcripts, phase0-smoke.jpg
- Tooling (artifacts-local): `drive-rerun4.ps1`, `deploy-rerun4.ps1`, `run-phase4.ps1`,
  `cleanup-rerun4.ps1`, `poll-rerun4.ps1`, `pull-rerun4.ps1`, cycles/summary/selftest helpers

### 12.11 Concerns

1. The latency gates remain failed (p95 241–594 ms vs the 15 ms Task-3 gate) even with the emit
   path healthy — the residual is the QSV emit depth at feed cadence plus the idle-flush feed-stamp
   artifacts (§11.2/§11.3); Task 3 owns this gap with the measured numbers in §12.6.
2. WorkingSet 413–414 MB sits above the 350 MB gate on both v2 runs (stable across 60 min — no
   growth trend: smoke 413.6 vs soak 413.9).
3. The resetloop tool's auxiliary stats.json roll-up and its never-incremented `cycles` counter
   mislabel switch_diag cycles as failing (§12.7) — any future reuse should fix the tool, not the
   gates; the mandated per-cycle gates are wire-measured and unaffected.
4. 8 h soak and lock/UAC transitions remain the two open PENDING rows (exact command recorded in
   §12.9); they must not be summarized as passed.
