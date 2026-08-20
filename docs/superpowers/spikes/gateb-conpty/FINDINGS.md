# XNC Gate B Spike — Go + Windows ConPTY Feasibility (THROWAWAY)

Date: 2026-08-19. Environment: Windows 11 (10.0.26200), Git Bash, Go 1.26.3 (CGO disabled, pure Go).
Shells tested: pwsh 7.6.4 (`C:\Users\LABS\AppData\Local\Microsoft\WindowsApps\pwsh.exe`) AND
powershell.exe 5.1 (`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`).

**Verdict up front: Go CAN wrap ConPTY reliably for an interactive PowerShell terminal. Both
candidates passed all 6 steps against BOTH shells.**

## Test matrix (all runs final binaries, 6-step harness per spike brief)

| Candidate | pwsh 7.6.4 | powershell 5.1 |
|---|---|---|
| A: UserExistsError/conpty v0.1.4 | 6/6 PASS | 6/6 PASS |
| B: direct x/sys/windows v0.47.0 | 6/6 PASS | 6/6 PASS |

Per-step results (identical for all four combinations):
1. create 80x25 + spawn: PASS (spawn ~200ms pwsh / ~15ms PS5.1; PID recorded)
2. `echo gateb-ok` round-trip: PASS (~450-510ms pwsh incl. boot; ~250-330ms PS5.1)
3. Ctrl+C (0x03) survival + `echo after-ctrlc`: PASS (~25ms)
4. Resize to 120x40 + `echo after-resize`: PASS (Resize returns nil, session live, ~25ms)
5. `exit` + bounded wait: PASS (exit code 0)
6. Close + residual check: PASS (recorded PID gone via tasklist; conhost count delta 0)

Raw output is VT-flavored as expected. Head bytes observed at session start:
`\x1b[?9001h` (win32-input-mode) `\x1b[?1004h` (focus reporting) `\x1b[?25l` (hide cursor)
`\x1b[2J\x1b[m\x1b[H` (clear/reset/home), then the prompt, plus `\x1b]0;...` OSC title.
Echo text arrives as contiguous ASCII (plain byte-search worked; PS5.1 did NOT emit UTF-16 —
ConPTY forces VT/UTF-8 rendering for both shells).

---

## Candidate A — github.com/UserExistsError/conpty v0.1.4

- **Works**: yes, first try, zero debugging.
- **Code I wrote**: 166 LoC total main.go, of which ConPTY-specific is **12 lines** (pure API
  call sites: `conpty.Start(shell, conpty.ConPtyDimensions(80,25))`, `.Write/.Read/.Resize/
  .Wait(ctx)/.Close()/.Pid()`). No Windows knowledge required. The library itself is 361 LoC
  (conpty.go, single file, MIT, copyright 2020).
- **Resize semantics**: `Resize(w, h)` → `ResizePseudoConsole`, packed COORD, returns error;
  live-resize mid-session works, no session disruption.
- **Lifecycle**: `Close()` = ClosePseudoConsole + closes process/thread handles and all 4 pipe
  ends ("terminate the process" per doc; on an already-exited child it is clean). `Wait(ctx)`
  returns exit code, polls WaitForSingleObject at 1s granularity → context-cancel latency up
  to 1s. No residual conhost/powershell after Close in our runs.
- **API rough edges**:
  - `Start` takes a raw command-line STRING (no exec.Cmd, no argv splitting) — caller must
    quote paths (pwsh lives in a path with no spaces, but e.g. `C:\Program Files\...` needs quotes).
  - Default env = inherit parent (env block nil); `ConPtyEnv` must replace the WHOLE env.
  - `Read` is a blocking raw-handle read (no Context/deadline) — caller must own a goroutine.
  - No way to detach process from pty (Close always tears the whole session down).
- **Dependency footprint** (`go mod graph`): `gateb/a → conpty@v0.1.4 → golang.org/x/sys@v0.8.0`.
  Two modules beyond std; the x/sys pin is from May 2023 (MVS would still honor a newer x/sys
  required elsewhere, so it doesn't hold the project hostage).
- **Maintenance risk**: LOW-MEDIUM. Single-purpose, MIT, tiny, stable. But effectively dormant:
  latest tag v0.1.4 released **2024-07-09** (2+ years old). It binds ConPTY via LazyProc because
  x/sys did not export ConPTY back then — that reason is now obsolete (see B).

## Candidate B — direct binding on golang.org/x/sys/windows v0.47.0

- **x/sys export check** (first thing we did): current x/sys **natively exports**
  `CreatePseudoConsole(Coord, Handle, Handle, uint32, *Handle) error`,
  `ResizePseudoConsole(Handle, Coord) error`, `ClosePseudoConsole(Handle)`, `Coord`,
  `StartupInfoEx` (proper STARTUPINFOEXW layout) and `NewProcThreadAttributeList` container
  with `.Update/.Delete/.List`. NOT exported (defined locally, 2 constants):
  `EXTENDED_STARTUPINFO_PRESENT` (0x00080000) and `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE`
  (0x00020016). No LazyProc needed at all.
- **Works**: yes, AFTER fixing two non-obvious bugs (below). Same behavior as A afterwards.
- **Code I wrote**: 277 LoC total main.go, of which ConPTY-specific wrapper is
  **112 lines raw / 91 lines non-blank-non-comment** (constants + pty struct + newPty + write +
  resize + waitExit + close). Harness identical to A.
- **Resize semantics**: `windows.ResizePseudoConsole(hpcon, Coord{X, Y})` — typed, returns
  error; identical live-resize behavior.
- **Lifecycle**: we control everything: WaitForSingleObject with real ms timeout (better than
  A's 1s-granularity ctx polling), GetExitCodeProcess, then ClosePseudoConsole + close handles
  + close our os.Files. Clean, no residuals. Closing the pty-side pipe ends immediately after
  CreatePseudoConsole is SAFE (verified empirically) — the pty duplicates them — and is the
  tidier wiring (fewer handles to track; EOF propagates correctly).
- **API rough edges / the two bugs that cost real time**:
  1. `UpdateProcThreadAttribute` lpValue must be **the HPCON value itself** (HPCON is `void*`
     in C). Passing `unsafe.Pointer(&hpcon)` (pointer to the variable — the pattern used for
     e.g. PARENT_PROCESS) makes the child die instantly with exit code **0xC0000142
     (STATUS_DLL_INIT_FAILED)** and no output. Fix:
     `hp := hpcon; hpPtr := *(*unsafe.Pointer)(unsafe.Pointer(&hp)); alist.Update(attr, hpPtr, unsafe.Sizeof(hp))`.
  2. `STARTF_USESTDHANDLES` must be set in StartupInfo.Flags (with zeroed std handles), as
     the conpty lib does — the MS sample omits it. WITHOUT it (parent running console-less /
     piped, as XNC will be): CreateProcess succeeds, the child runs, `exit` even returns code
     0 — but the child never attaches to the pty: output vanishes and the pwsh prompt leaks
     onto the PARENT's stdout. **Silent failure mode**; a naive spawn+exit smoke test passes.
  3. Minor: x/sys's `Update` stores the value in a `[]unsafe.Pointer` (GC keepalive list);
     for PSEUDOCONSOLE that value is a handle, not a Go pointer — works, but is conceptually
     GC-unclean; nothing observed in practice.
- **Dependency footprint** (`go mod graph`): `gateb/b → golang.org/x/sys@v0.47.0`. ONE module
  beyond std, released 2026-06-30, actively maintained by the Go project.
- **Maintenance risk**: LOW. The risky Windows knowledge is now in-repo code we own; the only
  upstream dep is golang.org/x/sys (quasi-stdlib, XNC almost certainly already depends on it).

---

## Recommendation for Phase 3

**Adopt Candidate B (direct x/sys binding) as in-repo code under agent/session; do not take
the third-party dependency.** Reasoning against the stated criteria (1-person team, in-repo
agent code, dependency minimalism):

1. The lib's original reason to exist (ConPTY not in x/sys) is gone. x/sys now ships the same
   API surface, typed. Using the lib today adds a dormant (last release Jul 2024) third-party
   module whose entire value is 361 LoC we can own — and XNC's wrapper is ~91 LoC.
2. Dependency minimalism: B needs exactly one module (x/sys), which XNC likely already has for
   other Windows work. A adds a module + transitively pins an x/sys requirement forever.
3. The spike already paid B's hidden cost: the two invariants (lpValue = handle value;
   STARTF_USESTDHANDLES) are documented above with exact symptoms and fixes, so Phase 3 can
   copy them deliberately instead of rediscovering them. In-repo code keeps that knowledge
   alive next to the sessions that depend on it; with the lib it stays tacit.
4. Lifecycle control: agent/session needs bounded waits (B has true ms-timeout waits vs A's
   1s-granularity polling), kill semantics, and careful teardown of `Write`/`Read` ends —
   owning the wrapper makes those first-class rather than fighting `Close()`-always-kills.

Fallback (acceptable, not preferred): if the team wants zero Windows-specific code in-repo,
Candidate A works today with 12 lines and is MIT/forkable — vendoring its single conpty.go
into agent/session would also be reasonable. Do NOT treat "spawn + exit works" as sufficient
validation for either path: always assert an echo round-trip (that is what exposed B's silent
detach).

### Notes for Phase 3 carry-over
- Prompt/input: write `\r` for Enter, `\x03` for Ctrl+C; both survive the session.
- Output parsing: expect `\x1b[?9001h`/`?1004h` handshake, cursor/erase sequences, OSC title
  writes; strip or pass through for the frontend. Both shells render UTF-8 via ConPTY.
- Read with a dedicated goroutine + polling accumulator (both libs give blocking reads only).
- Spawn latency: pwsh ~200ms+, PS5.1 ~15ms; budget echo assertions ≥3s for cold pwsh start.

## Artifacts
- `a/main.go` — Candidate A prototype (run: `cd a && go run .`; `XNC_SHELL="powershell.exe -NoLogo -NoProfile" go run .` for 5.1)
- `b/main.go` — Candidate B prototype (same usage)
- `a/go.mod`, `b/go.mod` — separate modules for clean per-candidate `go mod graph`
