# Task 4-5 Fix Report — screen-helper

Date: 2026-08-21
Branch: xnc-v2-phase6

## Critical: DXGI COM vtable slot offsets

All seven incorrect slots in `agent/screen-helper/capture_windows.go` corrected:

| Constant | Method | Old | New |
|---|---|---|---|
| `vtOutputGetDesc` | IDXGIOutput::GetDesc | 9 | 7 |
| `vtOutput1DuplicateOutput` | IDXGIOutput1::DuplicateOutput | 15 | 20 |
| `vtDupAcquireNextFrame` | IDXGIOutputDuplication::AcquireNextFrame | 4 | 8 |
| `vtDupReleaseFrame` | IDXGIOutputDuplication::ReleaseFrame | 10 | 12 |
| `vtDeviceCreateTexture2D` | ID3D11Device::CreateTexture2D | 7 | 5 |
| `vtContextMap` | ID3D11DeviceContext::Map | 34 | 14 |
| `vtContextCopyResource` | ID3D11DeviceContext::CopyResource | 9 | 47 |

`vtContextUnmap` updated 35 → 15 (kept adjacent to Map). Slot-derivation comments rewritten to reflect real interface hierarchies (IUnknown 0-2 base, ID3D11DeviceChild prefix for context, IDXGIOutput1 extending IDXGIOutput).

## ErrAccessLost recovery

Added `(*DXGICapturer).recreate()`: releases the duplication (after a best-effort `ReleaseFrame`), then re-calls `DuplicateOutput` on the retained `output1`/`device`. `captureLoop` now invokes `recreate()` on `ErrAccessLost` instead of just sleeping; on recreate failure it logs to stderr and backs off one second before retrying.

## Committed binary removed

- Deleted `agent/screen-helper/screen-helper` (2.9MB ELF, `git rm`).
- Added `agent/screen-helper/screen-helper` to `.gitignore`.

## Verification

`go build -o ../../bin/xnc-screen-helper.exe .` (windows), `GOOS=linux go build ./...`, `go vet ./...` — all pass.
