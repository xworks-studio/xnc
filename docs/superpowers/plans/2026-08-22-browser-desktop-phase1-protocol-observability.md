# Browser Desktop Phase 1: Protocol and Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Deliver a view-only desktop vertical slice in Chrome/Edge using the new desktop session protocol, the existing Go ScreenStreamManager as a temporary video adapter, and browser-to-agent video feedback.

**Architecture:** Server adds a desktop session endpoint but keeps the existing opaque WebSocket relay. Agent registers a DesktopPreviewHandler that waits for CLIENT_HELLO, subscribes to the existing shared screen stream, wraps H.264 access units in the versioned 40-byte desktop header, and consumes VIDEO_ACK without accepting input. The Web UI adds an experimental Desktop page with WebCodecs decoding, generation filtering, render metrics, and 500ms cumulative ACK.

**Tech Stack:** Go 1.26, coder/websocket, React 19, TypeScript 6, WebCodecs, Vitest, Testing Library

**Spec:** docs/superpowers/specs/2026-08-22-rustdesk-inspired-browser-desktop-design.md

## Global Constraints

- Do not copy, modify, link, or translate RustDesk AGPLv3 source code.
- Browser support is desktop Chrome and Edge only.
- All node and browser connections remain outbound or client-initiated over HTTPS/WSS; no local TCP listener is added.
- Server remains an opaque H.264 relay and does not transcode video.
- Phase 1 is view-only. INPUT_* messages must be rejected with INPUT_DISABLED.
- Phase 1 does not implement Rust Desktop Host, SYSTEM interactive-session launching, control leases, session reattach, cursor separation, or production QoS adaptation.
- Existing xnc screen and native xnc rdp behavior remain unchanged.
- The new Browser Desktop entry is labeled Experimental and does not replace the existing Remote Desktop action.
- desktop creation uses the repository’s current operator-or-owner session authorization. Fine-grained desktop.view and desktop.control permissions belong to the control-lease phase.
- Phase 1 generation is always 1. Reattachment and generation increments belong to the recovery phase.
- Phase 1 timestamps are Agent receipt-time approximations and DESKTOP_BEGIN must advertise legacyTiming=true.
- Preserve all pre-existing uncommitted changes under tools/screendiag; do not stage or rewrite them. The final experiment may update only docs/rdp-architecture-exploration.md.

## Program Decomposition

This is plan 1 of 5:

1. Protocol and observable view-only vertical slice — this plan.
2. Rust Host supervisor, protected IPC, leases, and packaging.
3. Rust DXGI/GDI capture, Media Foundation H.264, shared VideoService, and adaptive QoS.
4. Cursor separation, keyboard/mouse/text input, control lease, UAC, and lock-screen behavior.
5. Logical-session reattach, generation recovery, gray rollout, CLI entry switch, and legacy removal.

Each plan must leave a working, independently testable system. Plan 2 consumes the protocol interfaces defined here and replaces only the Agent-side legacy adapter.

## File Structure

### Protocol module

- Modify proto/session.go — add KindDesktop only.
- Create proto/desktop.go — desktop JSON vocabulary, H.264 profile constants, fixed binary header, marshal/unmarshal validation.
- Create proto/desktop_test.go — byte-exact header and JSON round-trip tests.

### Server module

- Create server/internal/api/desktop_handlers.go — validate POST /desktop and call startSession.
- Create server/internal/api/desktop_handlers_test.go — defaults, bounds, SESSION_OPEN, RBAC, and audit tests.
- Modify server/internal/api/router.go — register POST /api/nodes/{id}/desktop.

### Agent module

- Create agent/session/desktop_preview.go — view-only legacy adapter and feedback reader.
- Create agent/session/desktop_preview_test.go — CLIENT_HELLO, keyframe gating, header, ACK, and input rejection tests.
- Modify agent/agent.go — register KindDesktop.

### Web module

- Modify web/package.json and web/package-lock.json — add Vitest, jsdom, and Testing Library.
- Modify web/vite.config.ts — jsdom test environment and setup file.
- Create web/src/test/setup.ts — jest-dom matchers.
- Create web/src/desktop/protocol.ts — control types and 40-byte frame parser.
- Create web/src/desktop/protocol.test.ts — byte parsing and validation tests.
- Create web/src/desktop/metrics.ts — cumulative frame and FPS metrics.
- Create web/src/desktop/metrics.test.ts — deterministic rolling-window tests.
- Create web/src/desktop/session.ts — WebSocket/WebCodecs session state machine.
- Create web/src/desktop/session.test.ts — fake socket and fake decoder tests.
- Create web/src/pages/Desktop.tsx — thin React page around the session state machine.
- Create web/src/pages/Desktop.test.tsx — page lifecycle and status rendering tests.
- Modify web/src/main.tsx — add /desktop/:id.
- Modify web/src/pages/NodeDetail.tsx — add Browser Desktop (Experimental).
- Modify web/src/styles.css — desktop canvas and metrics bar.

### Documentation

- Modify docs/rdp-architecture-exploration.md — append measured Phase 1 results only after real E2E verification.

---

### Task 1: Desktop protocol vocabulary and binary envelope

**Files:**

- Modify: proto/session.go near KindScreen
- Create: proto/desktop.go
- Create: proto/desktop_test.go

**Interfaces:**

- Produces constant KindDesktop = "desktop".
- Produces DesktopProtocolVersion uint8 = 1 and DesktopVideoHeaderSize = 40.
- Produces string constants DesktopTypeClientHello = "CLIENT_HELLO", DesktopTypeBegin = "DESKTOP_BEGIN", DesktopTypeCodecConfig = "CODEC_CONFIG", DesktopTypeSessionState = "SESSION_STATE", DesktopTypeVideoAck = "VIDEO_ACK", DesktopTypeRequestKeyframe = "REQUEST_KEYFRAME", DesktopTypeViewportState = "VIEWPORT_STATE", and DesktopTypeError = "ERROR".
- Produces DesktopParams with MaxFps, Quality, MaxWidth, and DisplayID as int fields and JSON names maxFps, quality, maxWidth, and displayId.
- Produces DesktopClientHello with ProtocolVersion int, SupportedH264Profiles []string, MaxWidth int, MaxHeight int, and MaxFps int.
- Produces DesktopBegin with ProtocolVersion int, Generation uint32, Capabilities []string, HostMonoOriginUs uint64, and LegacyTiming bool.
- Produces DesktopCodecConfig with Generation uint32, DisplayID int, Codec string, Profile string, Width int, and Height int.
- Produces DesktopVideoAck with Generation uint32, DisplayID int, ReceivedSeq uint32, DecodedSeq uint32, RenderedSeq uint32, DecodeQueueSize int, DecodeFps float64, and Visible bool.
- Produces DesktopRequestKeyframe with Generation uint32, DisplayID int, and Reason string; produces DesktopViewportState with Visible bool, CanvasWidth int, CanvasHeight int, and DevicePixelRatio float64.
- Produces DesktopSessionState with State string, Reason string, and Retry bool; produces DesktopError with StableCode string, Message string, and Recoverable bool.
- Produces DesktopVideoHeader with ProtocolVersion uint8, Flags uint16, Generation uint32, DisplayID uint16, FrameSeq uint32, CapturedMonoUs uint64, and EncodedMonoUs uint64, plus MarshalDesktopVideoFrame / UnmarshalDesktopVideoFrame.

- [ ] **Step 1: Write failing protocol tests**

Add tests that require exact bytes, not merely a round trip:

~~~go
func TestDesktopVideoFrameWireLayout(t *testing.T) {
    h := DesktopVideoHeader{
        ProtocolVersion: 1,
        Flags: DesktopVideoFlagKey,
        Generation: 7,
        DisplayID: 2,
        FrameSeq: 99,
        CapturedMonoUs: 123456,
        EncodedMonoUs: 123999,
    }
    payload := []byte{0, 0, 0, 1, 0x65, 0xAA}
    wire, err := MarshalDesktopVideoFrame(h, payload)
    require.NoError(t, err)
    require.Len(t, wire, DesktopVideoHeaderSize+len(payload))
    assert.Equal(t, []byte{'X', 'N', 'C', 'D'}, wire[0:4])
    assert.Equal(t, byte(1), wire[4])
    assert.Equal(t, byte(DesktopMessageVideo), wire[5])
    assert.Equal(t, uint32(7), binary.LittleEndian.Uint32(wire[8:12]))
    assert.Equal(t, uint32(99), binary.LittleEndian.Uint32(wire[16:20]))
    assert.Equal(t, uint32(len(payload)), binary.LittleEndian.Uint32(wire[36:40]))
    gotHeader, gotPayload, err := UnmarshalDesktopVideoFrame(wire)
    require.NoError(t, err)
    assert.Equal(t, h.Generation, gotHeader.Generation)
    assert.Equal(t, payload, gotPayload)
}

func TestDesktopVideoFrameRejectsMalformedInput(t *testing.T) {
    _, _, err := UnmarshalDesktopVideoFrame(make([]byte, 39))
    require.ErrorContains(t, err, "short desktop frame")

    badMagic := make([]byte, 40)
    _, _, err = UnmarshalDesktopVideoFrame(badMagic)
    require.ErrorContains(t, err, "desktop magic")
}

func TestDesktopControlVocabulary(t *testing.T) {
    hello := DesktopClientHello{
        ProtocolVersion: 1,
        SupportedH264Profiles: []string{"avc1.42E01F", "avc1.4D4028", "avc1.4D4032"},
        MaxWidth: 1920, MaxHeight: 1080, MaxFps: 30,
    }
    msg, err := NewMsg(DesktopTypeClientHello, hello)
    require.NoError(t, err)
    b, err := json.Marshal(msg)
    require.NoError(t, err)
    assert.Contains(t, string(b), "\"CLIENT_HELLO\"")
    assert.Contains(t, string(b), "\"supportedH264Profiles\"")
}
~~~

- [ ] **Step 2: Run tests and verify the missing API failure**

Run:

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\proto
go test ./... -run Desktop -v
~~~

Expected: compilation fails because DesktopVideoHeader and related types do not exist.

- [ ] **Step 3: Implement the protocol module**

Use a byte array for magic and little-endian integers. The marshal function must set messageKind to 1 and derive payloadLength from payload instead of trusting caller input.

Core implementation:

~~~go
const (
    DesktopProtocolVersion uint8 = 1
    DesktopVideoHeaderSize       = 40
    DesktopMessageVideo    uint8 = 1
    DesktopVideoFlagKey    uint16 = 1
)

var desktopVideoMagic = [4]byte{'X', 'N', 'C', 'D'}

func MarshalDesktopVideoFrame(h DesktopVideoHeader, payload []byte) ([]byte, error) {
    if h.ProtocolVersion != DesktopProtocolVersion {
        return nil, fmt.Errorf("desktop protocol version: %d", h.ProtocolVersion)
    }
    if len(payload) > MaxSessionFrameBytes-DesktopVideoHeaderSize {
        return nil, fmt.Errorf("desktop payload too large: %d", len(payload))
    }
    out := make([]byte, DesktopVideoHeaderSize+len(payload))
    copy(out[0:4], desktopVideoMagic[:])
    out[4] = h.ProtocolVersion
    out[5] = DesktopMessageVideo
    binary.LittleEndian.PutUint16(out[6:8], h.Flags)
    binary.LittleEndian.PutUint32(out[8:12], h.Generation)
    binary.LittleEndian.PutUint16(out[12:14], h.DisplayID)
    binary.LittleEndian.PutUint32(out[16:20], h.FrameSeq)
    binary.LittleEndian.PutUint64(out[20:28], h.CapturedMonoUs)
    binary.LittleEndian.PutUint64(out[28:36], h.EncodedMonoUs)
    binary.LittleEndian.PutUint32(out[36:40], uint32(len(payload)))
    copy(out[40:], payload)
    return out, nil
}
~~~

UnmarshalDesktopVideoFrame must validate, in order: minimum length, magic, version, message kind, reserved bytes 14–15 are zero, payload length equals the remaining WebSocket frame length, and total size is within MaxSessionFrameBytes. Return a copied payload slice so callers do not retain an oversized WebSocket buffer.

JSON field names must exactly match the interface list above.

- [ ] **Step 4: Format and run protocol tests**

Run:

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\proto
gofmt -w session.go desktop.go desktop_test.go
go test ./... -v
~~~

Expected: all proto tests pass.

- [ ] **Step 5: Commit**

~~~powershell
git add proto/session.go proto/desktop.go proto/desktop_test.go
git commit -m "feat(proto): define desktop session wire protocol"
~~~

---

### Task 2: Server desktop endpoint

**Files:**

- Create: server/internal/api/desktop_handlers.go
- Create: server/internal/api/desktop_handlers_test.go
- Modify: server/internal/api/router.go near the screen route

**Interfaces:**

- Consumes proto.KindDesktop and proto.DesktopParams from Task 1.
- Produces POST /api/nodes/{id}/desktop.
- Defaults maxFps=15, quality=60, maxWidth=1920, displayId=0.
- Validates maxFps 1–30, quality 1–100, maxWidth 1–1920, displayId 0–31.
- Uses audit actions desktop.open and desktop.close.

- [ ] **Step 1: Write failing handler tests**

Model the test on screen_handlers_test.go, but assert desktop-specific parameters:

~~~go
func TestDesktopSessionEndToEnd(t *testing.T) {
    env := NewTestEnv(t)
    nodeID := env.EnrollNode(t, "WEB-DESK1", "mid-desk1")
    ctrl := dialControl(t, env, nodeID)
    openCh := captureSessionOpen(t, ctrl)

    resp := desktopPost(t, env, nodeID, "{}")
    defer resp.Body.Close()
    require.Equal(t, http.StatusAccepted, resp.StatusCode)

    select {
    case so := <-openCh:
        require.Equal(t, proto.KindDesktop, so.Kind)
        var p proto.DesktopParams
        require.NoError(t, jsonUnmarshal(so.Params, &p))
        assert.Equal(t, 15, p.MaxFps)
        assert.Equal(t, 60, p.Quality)
        assert.Equal(t, 1920, p.MaxWidth)
        assert.Equal(t, 0, p.DisplayID)
    case <-time.After(3 * time.Second):
        t.Fatal("no desktop SESSION_OPEN")
    }
}

func TestDesktopBounds(t *testing.T) {
    env := NewTestEnv(t)
    nodeID := env.EnrollNode(t, "WEB-DESK2", "mid-desk2")
    _ = dialControl(t, env, nodeID)
    for _, body := range []string{
        "{\"maxFps\":31}",
        "{\"quality\":101}",
        "{\"maxWidth\":1921}",
        "{\"displayId\":32}",
    } {
        resp := desktopPost(t, env, nodeID, body)
        resp.Body.Close()
        assert.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
    }
}
~~~

Add a desktop RBAC test with owner=202, operator=202, viewer=403, outsider=404, matching current startSession semantics. Add an audit assertion for desktop.open.

- [ ] **Step 2: Run the focused server test**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\server
go test ./internal/api -run Desktop -v
~~~

Expected: compilation fails because desktopPost and desktopStart do not exist.

- [ ] **Step 3: Implement the handler and route**

desktopStart must use http.MaxBytesReader with a 4KiB maximum, reject malformed JSON, normalize zero values, validate the four fields, marshal proto.DesktopParams, and call:

~~~go
h.startSession(
    w,
    r,
    proto.KindDesktop,
    params,
    "desktop.open",
    "desktop.close",
)
~~~

Register:

~~~go
nr.Post("/{id}/desktop", h.desktopStart)
~~~

Do not modify startSession or the session Manager in Phase 1.

- [ ] **Step 4: Format and run Server tests**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\server
gofmt -w internal/api/desktop_handlers.go internal/api/desktop_handlers_test.go internal/api/router.go
go test ./internal/api -run "Desktop|Screen" -v
go test ./...
~~~

Expected: all Server tests pass; existing screen behavior remains green.

- [ ] **Step 5: Commit**

~~~powershell
git add server/internal/api/desktop_handlers.go server/internal/api/desktop_handlers_test.go server/internal/api/router.go
git commit -m "feat(server): add experimental desktop sessions"
~~~

---

### Task 3: Agent legacy desktop adapter

**Files:**

- Create: agent/session/desktop_preview.go
- Create: agent/session/desktop_preview_test.go
- Modify: agent/agent.go near screen handler registration

**Interfaces:**

- Consumes DesktopParams and desktop wire types from Task 1.
- Consumes ScreenStreamManager.Subscribe, Unsubscribe, State, and ScreenFrame.
- Produces NewDesktopPreviewHandler() returning a session.Handler.
- Produces a view-only desktop session with generation=1 and displayId from DesktopParams.
- Accepts CLIENT_HELLO, VIDEO_ACK, REQUEST_KEYFRAME, and VIEWPORT_STATE.
- Rejects INPUT_* with ERROR stableCode=INPUT_DISABLED.
- Advertises capabilities legacyScreenAdapter and legacyTiming.
- Produces desktopCodecFromKeyframe(frame []byte) (string, error), which scans Annex-B NAL units for SPS type 7, requires the three profile/compatibility/level bytes after the NAL header, and returns an uppercase RFC 6381 codec string such as avc1.4D4032.

- [ ] **Step 1: Write failing adapter tests**

Reuse newMockScreenManager from screen_test.go and add a desktop WebSocket helper. Tests must send CLIENT_HELLO before expecting output.

Keyframe-gating test:

~~~go
func TestDesktopPreviewWaitsForHelloAndKeyframe(t *testing.T) {
    m, mock := newMockScreenManager(t)
    ws := runDesktopPreviewSession(t, m, "d1", proto.DesktopParams{
        MaxFps: 15, Quality: 60, MaxWidth: 1920, DisplayID: 0,
    })

    writeDesktopText(t, ws, proto.DesktopTypeClientHello, proto.DesktopClientHello{
        ProtocolVersion: 1,
        SupportedH264Profiles: []string{"avc1.4D4032"},
        MaxWidth: 1920, MaxHeight: 1080, MaxFps: 30,
    })

    begin := readDesktopText(t, ws)
    require.Equal(t, proto.DesktopTypeBegin, begin.Type)

    mock.write(screenFrameDelta, mockP)
    assertNoDesktopFrame(t, ws, 100*time.Millisecond)

    desktopSPS := nalu(0x67, 0x4D, 0x40, 0x32)
    key := append(append(append([]byte{}, desktopSPS...), mockPPS...), mockIDR...)
    mock.write(screenFrameKey, key)

    cfg := readDesktopText(t, ws)
    require.Equal(t, proto.DesktopTypeCodecConfig, cfg.Type)
    kind, wire := readFrame(t, ws)
    require.Equal(t, "binary", kind)
    h, payload, err := proto.UnmarshalDesktopVideoFrame(wire)
    require.NoError(t, err)
    assert.Equal(t, uint32(1), h.Generation)
    assert.Equal(t, uint32(1), h.FrameSeq)
    assert.NotZero(t, h.Flags&proto.DesktopVideoFlagKey)
    assert.Equal(t, key, payload)
}
~~~

Add tests for:

- first message other than CLIENT_HELLO returns CLIENT_HELLO_REQUIRED;
- a live key frame whose SPS codec is absent from SupportedH264Profiles returns CODEC_UNSUPPORTED and closes the session;
- VIDEO_ACK is accepted without closing the stream;
- REQUEST_KEYFRAME causes deltas to be skipped until the next keyframe;
- INPUT_MOUSE returns INPUT_DISABLED;
- frameSeq increments only for transmitted video frames;
- DesktopBegin.LegacyTiming is true.

- [ ] **Step 2: Run focused Agent tests**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\agent
go test ./session -run DesktopPreview -v
~~~

Expected: compilation fails because DesktopPreviewHandler does not exist.

- [ ] **Step 3: Implement the adapter state machine**

Define:

~~~go
type DesktopPreviewHandler struct {
    Manager *ScreenStreamManager
    now     func() time.Time
}

func NewDesktopPreviewHandler() *DesktopPreviewHandler {
    return &DesktopPreviewHandler{
        Manager: GetScreenStreamManager(),
        now: time.Now,
    }
}
~~~

Handle must:

1. Read and validate CLIENT_HELLO before subscribing.
2. Reject protocol versions other than 1 and an empty supported H.264 profile list.
3. Normalize DesktopParams and map them to ScreenParams.
4. Subscribe once and defer Unsubscribe.
5. Send DESKTOP_BEGIN with generation 1, hostMonoOriginUs 0, legacyTiming true, and capabilities legacyScreenAdapter/legacyTiming.
6. Start exactly one reader goroutine that converts Browser messages into a 256-entry bounded internal event channel. That goroutine never writes to WebSocket; if the channel is full, coalesce or drop VIDEO_ACK/VIEWPORT_STATE updates but never block video delivery waiting on stale feedback.
7. Keep all WebSocket writes in the Handle goroutine.
8. Drop all delta frames while awaitingKey is true.
9. On a key frame, call desktopCodecFromKeyframe. If its codec is absent from CLIENT_HELLO.supportedH264Profiles, send DesktopError{StableCode:"CODEC_UNSUPPORTED", Recoverable:false} and close; otherwise send CODEC_CONFIG if changed, then send the binary video frame.
10. On REQUEST_KEYFRAME set awaitingKey=true. The legacy adapter cannot force the existing encoder; it waits for the next naturally emitted key frame.
11. On INPUT_* enqueue DesktopError{StableCode:"INPUT_DISABLED", Recoverable:false} on the control path.

Use a session-relative start time:

~~~go
elapsedUs := uint64(h.now().Sub(startedAt).Microseconds())
header := proto.DesktopVideoHeader{
    ProtocolVersion: proto.DesktopProtocolVersion,
    Generation: 1,
    DisplayID: uint16(params.DisplayID),
    FrameSeq: nextSeq,
    CapturedMonoUs: elapsedUs,
    EncodedMonoUs: elapsedUs,
}
~~~

Set DesktopVideoFlagKey only for screenFrameKey. Do not use CachedKeyFrame; a newly joined desktop viewer must wait for a live keyframe.

Register:

~~~go
engine.Register(proto.KindDesktop, session.NewDesktopPreviewHandler())
~~~

- [ ] **Step 4: Format and run Agent tests**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\agent
gofmt -w agent.go session/desktop_preview.go session/desktop_preview_test.go
go test ./session -run "DesktopPreview|Screen" -v
go test ./...
~~~

Expected: desktop and existing screen tests pass. No input injection code exists.

- [ ] **Step 5: Commit**

~~~powershell
git add agent/agent.go agent/session/desktop_preview.go agent/session/desktop_preview_test.go
git commit -m "feat(agent): adapt legacy screen stream to desktop protocol"
~~~

---

### Task 4: Web protocol parser, metrics, and test harness

**Files:**

- Modify: web/package.json
- Modify: web/package-lock.json
- Modify: web/vite.config.ts
- Modify: web/tsconfig.app.json
- Create: web/src/test/setup.ts
- Create: web/src/desktop/protocol.ts
- Create: web/src/desktop/protocol.test.ts
- Create: web/src/desktop/metrics.ts
- Create: web/src/desktop/metrics.test.ts

**Interfaces:**

- Produces DESKTOP_HEADER_SIZE = 40.
- Produces parseDesktopVideoFrame(buffer: ArrayBuffer): DesktopVideoFrame.
- Produces probeH264Profiles(): Promise<string[]>.
- Produces makeClientHello(profiles, viewport): DesktopControlEnvelope.
- Produces DesktopMetrics with markReceived, markDecoded, markRendered, and snapshot.

- [ ] **Step 1: Add the test runner**

Run:

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\web
npm install --save-dev vitest jsdom @testing-library/react @testing-library/jest-dom
~~~

Add scripts:

~~~json
{
  "test": "vitest run",
  "test:watch": "vitest"
}
~~~

Configure Vite:

~~~ts
export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
  },
  server: {
    proxy: {
      "/api": { target: "http://localhost:8080", ws: true },
    },
  },
});
~~~

Add vitest/globals to tsconfig.app.json types and import @testing-library/jest-dom/vitest in setup.ts.

- [ ] **Step 2: Write failing parser and metrics tests**

~~~ts
it("parses the fixed little-endian video header", () => {
  const bytes = new Uint8Array(46);
  bytes.set([0x58, 0x4e, 0x43, 0x44], 0);
  const view = new DataView(bytes.buffer);
  view.setUint8(4, 1);
  view.setUint8(5, 1);
  view.setUint16(6, 1, true);
  view.setUint32(8, 7, true);
  view.setUint16(12, 2, true);
  view.setUint32(16, 99, true);
  view.setBigUint64(20, 123456n, true);
  view.setBigUint64(28, 123999n, true);
  view.setUint32(36, 6, true);
  bytes.set([0, 0, 0, 1, 0x65, 0xaa], 40);

  const frame = parseDesktopVideoFrame(bytes.buffer);
  expect(frame.generation).toBe(7);
  expect(frame.frameSeq).toBe(99);
  expect(frame.key).toBe(true);
  expect([...frame.payload]).toEqual([0, 0, 0, 1, 0x65, 0xaa]);
});

it("reports cumulative sequence and rolling fps", () => {
  const metrics = new DesktopMetrics(() => 1000);
  metrics.markReceived(4);
  metrics.markDecoded(4);
  metrics.markRendered(4);
  const snap = metrics.snapshot(0, true);
  expect(snap.receivedSeq).toBe(4);
  expect(snap.decodedSeq).toBe(4);
  expect(snap.renderedSeq).toBe(4);
  expect(snap.visible).toBe(true);
});
~~~

Also test bad magic, short frame, payload mismatch, unsupported version, and a sequence regression that must not lower cumulative ACK values.

- [ ] **Step 3: Implement parser and metrics**

parseDesktopVideoFrame must validate the same fields and order as Go:

~~~ts
export function parseDesktopVideoFrame(buffer: ArrayBuffer): DesktopVideoFrame {
  if (buffer.byteLength < DESKTOP_HEADER_SIZE) {
    throw new Error("short desktop frame");
  }
  const bytes = new Uint8Array(buffer);
  if (bytes[0] !== 0x58 || bytes[1] !== 0x4e || bytes[2] !== 0x43 || bytes[3] !== 0x44) {
    throw new Error("invalid desktop magic");
  }
  const view = new DataView(buffer);
  if (view.getUint8(4) !== 1 || view.getUint8(5) !== 1) {
    throw new Error("unsupported desktop frame");
  }
  if (view.getUint16(14, true) !== 0) {
    throw new Error("reserved desktop bits");
  }
  const payloadLength = view.getUint32(36, true);
  if (DESKTOP_HEADER_SIZE + payloadLength !== buffer.byteLength) {
    throw new Error("desktop payload length mismatch");
  }
  return {
    key: (view.getUint16(6, true) & 1) !== 0,
    generation: view.getUint32(8, true),
    displayId: view.getUint16(12, true),
    frameSeq: view.getUint32(16, true),
    capturedMonoUs: Number(view.getBigUint64(20, true)),
    encodedMonoUs: Number(view.getBigUint64(28, true)),
    payload: bytes.slice(DESKTOP_HEADER_SIZE),
  };
}
~~~

probeH264Profiles must call VideoDecoder.isConfigSupported for exactly avc1.42E01F, avc1.4D4028, and avc1.4D4032 and return supported entries in that order. The third value covers the Main Profile Level 5.0 stream observed in the current screen-helper experiment. Metrics use an injected clock and retain one second of render timestamps.

- [ ] **Step 4: Run Web unit tests and TypeScript build**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\web
npm test
npm run build
npm run lint
~~~

Expected: protocol and metrics tests pass; TypeScript and lint are clean.

- [ ] **Step 5: Commit**

~~~powershell
git add web/package.json web/package-lock.json web/vite.config.ts web/tsconfig.app.json web/src/test/setup.ts web/src/desktop/protocol.ts web/src/desktop/protocol.test.ts web/src/desktop/metrics.ts web/src/desktop/metrics.test.ts
git commit -m "feat(web): add desktop protocol and video metrics"
~~~

---

### Task 5: Browser desktop session state machine

**Files:**

- Create: web/src/desktop/session.ts
- Create: web/src/desktop/session.test.ts

**Interfaces:**

- Consumes parseDesktopVideoFrame, probeH264Profiles, and DesktopMetrics from Task 4.
- Produces startDesktopSession(options): Promise<DesktopSessionHandle>.
- DesktopSessionOptions contains socket, canvas, maxWidth, maxHeight, maxFps, onState, onMetrics, decoderFactory, chunkFactory, and setInterval/clearInterval injection points.
- DesktopSessionHandle exposes close(): void.

- [ ] **Step 1: Write fake-socket state machine tests**

Create a FakeSocket with sent, onopen, onmessage, onclose, close, and emit methods. Create a FakeDecoder with configured profiles, decoded chunks, decodeQueueSize, and an explicit emitFrame callback.

Tests:

~~~ts
it("sends CLIENT_HELLO after capability probing", async () => {
  const fx = makeSessionFixture(["avc1.4D4032"]);
  const sessionPromise = startDesktopSession(fx.options);
  fx.socket.open();
  await sessionPromise;
  expect(JSON.parse(fx.socket.sent[0] as string)).toMatchObject({
    type: "CLIENT_HELLO",
    payload: {
      protocolVersion: 1,
      supportedH264Profiles: ["avc1.4D4032"],
      maxFps: 30,
    },
  });
});

it("drops frames from the wrong generation", async () => {
  const fx = await openStreamingFixture(3);
  fx.socket.binary(videoWire({ generation: 2, frameSeq: 1, key: true }));
  expect(fx.decoder.decoded).toHaveLength(0);
  fx.socket.binary(videoWire({ generation: 3, frameSeq: 1, key: true }));
  expect(fx.decoder.decoded).toHaveLength(1);
});

it("sends cumulative VIDEO_ACK every 500ms", async () => {
  const fx = await openStreamingFixture(1);
  fx.socket.binary(videoWire({ generation: 1, frameSeq: 8, key: true }));
  fx.decoder.emitFrame(8);
  fx.tick500ms();
  const ack = JSON.parse(fx.socket.sent.at(-1) as string);
  expect(ack.type).toBe("VIDEO_ACK");
  expect(ack.payload.renderedSeq).toBe(8);
});
~~~

Also test: CODEC_UNSUPPORTED when probe returns empty; decoder configure on CODEC_CONFIG; decoder reset on generation change; REQUEST_KEYFRAME on decode error; cleanup closes decoder, timer, frames, and socket.

- [ ] **Step 2: Run the focused tests**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\web
npx vitest run src/desktop/session.test.ts
~~~

Expected: failure because startDesktopSession does not exist.

- [ ] **Step 3: Implement the state machine**

The production decoder factory wraps VideoDecoder. Maintain a pending timestamp-to-frameSeq map when decode is submitted; the output callback resolves frameSeq from VideoFrame.timestamp, marks decoded/rendered, draws onto the supplied 2D canvas context, deletes the map entry, and closes VideoFrame in a finally block. Clear the map whenever the decoder resets or the session closes.

Rules:

- Do not create the decoder until CODEC_CONFIG.
- Configure H.264 without a description field so EncodedVideoChunk data is interpreted as Annex-B; key chunks already carry SPS/PPS by protocol contract.
- Encode chunk timestamp as capturedMonoUs minus the first capturedMonoUs observed in the current generation.
- Ignore binary frames until DESKTOP_BEGIN and CODEC_CONFIG have both been received.
- Ignore any generation other than the active one.
- If a non-key frame arrives while awaitingKey is true, drop it.
- On a key frame, clear awaitingKey before decode.
- On decoder error, set awaitingKey, send REQUEST_KEYFRAME with reason decoder_error, and call decoder.reset if available.
- Rate-limit REQUEST_KEYFRAME to one request per 500ms even if multiple decode errors occur.
- ACK payload uses cumulative maximum sequence values and current decoder.decodeQueueSize.
- document.visibilityState determines visible.

Use JSON.stringify for control envelopes and keep one WebSocket message handler.

- [ ] **Step 4: Run all Web tests and build**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\web
npm test
npm run build
npm run lint
~~~

Expected: all Web tests pass.

- [ ] **Step 5: Commit**

~~~powershell
git add web/src/desktop/session.ts web/src/desktop/session.test.ts
git commit -m "feat(web): implement observable desktop session"
~~~

---

### Task 6: Experimental Browser Desktop page

**Files:**

- Create: web/src/pages/Desktop.tsx
- Create: web/src/pages/Desktop.test.tsx
- Modify: web/src/main.tsx
- Modify: web/src/pages/NodeDetail.tsx
- Modify: web/src/styles.css

**Interfaces:**

- Consumes POST /api/nodes/{id}/desktop from Task 2.
- Consumes startDesktopSession from Task 5.
- Produces route /desktop/:id.
- Shows state, dimensions, generation, receive/decode/render FPS, queue depth, and last rendered sequence.

- [ ] **Step 1: Write the page test**

Mock api and startDesktopSession:

~~~tsx
it("starts an experimental desktop session and renders metrics", async () => {
  vi.mocked(api).mockResolvedValue({
    sessionId: "s1",
    websocketUrl: "/api/session/s1?token=t1",
  });
  vi.mocked(startDesktopSession).mockImplementation(async (options) => {
    options.onState({ state: "capturing", generation: 1, width: 1920, height: 1080 });
    options.onMetrics({
      receivedSeq: 10,
      decodedSeq: 10,
      renderedSeq: 9,
      decodeQueueSize: 1,
      decodeFps: 30,
      renderFps: 29,
      visible: true,
    });
    return { close: vi.fn() };
  });

  render(
    <MemoryRouter initialEntries={["/desktop/node-1"]}>
      <Routes>
        <Route path="/desktop/:id" element={<Desktop />} />
      </Routes>
    </MemoryRouter>,
  );

  expect(await screen.findByText("capturing")).toBeInTheDocument();
  expect(screen.getByText("1920×1080")).toBeInTheDocument();
  expect(screen.getByText(/render 29 fps/i)).toBeInTheDocument();
});
~~~

Add a cleanup assertion that unmount calls session.close, and an API failure assertion that renders an alert.

- [ ] **Step 2: Run the page test**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\web
npx vitest run src/pages/Desktop.test.tsx
~~~

Expected: failure because Desktop.tsx does not exist.

- [ ] **Step 3: Implement page, route, and entry**

Desktop.tsx must:

- fetch the node name as best-effort;
- POST an empty JSON object to /api/nodes/{id}/desktop;
- convert relative websocketUrl to ws/wss using the existing ScreenPreview rule;
- create a WebSocket and pass it with the canvas to startDesktopSession;
- render a visible Experimental badge;
- render only viewing controls; no keyboard or pointer listeners;
- close the session on unmount.

Add:

~~~tsx
<Route path="/desktop/:id" element={<Desktop />} />
~~~

Add to NodeDetail without removing existing actions:

~~~tsx
<Link className="btn" to={"/desktop/" + node.id}>
  Browser Desktop (Experimental)
</Link>
~~~

Reuse the screen-preview layout where practical, but use desktop-preview class names so the production Desktop UI can evolve independently.

- [ ] **Step 4: Run Web tests, build, and lint**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\web
npm test
npm run build
npm run lint
~~~

Expected: all Web tests pass and the production bundle builds.

- [ ] **Step 5: Commit**

~~~powershell
git add web/src/pages/Desktop.tsx web/src/pages/Desktop.test.tsx web/src/main.tsx web/src/pages/NodeDetail.tsx web/src/styles.css
git commit -m "feat(web): add experimental browser desktop page"
~~~

---

### Task 7: Cross-module regression and real-node experiment

**Files:**

- Modify after measurement: docs/rdp-architecture-exploration.md
- Do not stage unrelated tools/screendiag changes.

**Interfaces:**

- Consumes the complete Phase 1 vertical slice.
- Produces repeatable test evidence for first-frame time, rendered FPS, decode queue, ACK cadence, IDR count, and failure mode.

- [ ] **Step 1: Run repository-level automated verification**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\proto
go test ./...
Set-Location C:\Users\LABS\Desktop\XNC\server
go test ./...
Set-Location C:\Users\LABS\Desktop\XNC\agent
go test ./...
Set-Location C:\Users\LABS\Desktop\XNC\web
npm test
npm run build
npm run lint
~~~

Expected: every command exits 0.

- [ ] **Step 2: Build local artifacts**

~~~powershell
Set-Location C:\Users\LABS\Desktop\XNC\agent
go build -o ..\bin\xnc-agent.exe .\cmd\xnc-agent
Set-Location C:\Users\LABS\Desktop\XNC\agent\screen-helper
go build -o ..\..\bin\xnc-screen-helper.exe .
Set-Location C:\Users\LABS\Desktop\XNC\server
go build -o ..\bin\xnc-server.exe .\cmd\xnc-server
~~~

Expected: all three binaries build. The screen-helper build must preserve current local encoder experiment edits.

- [ ] **Step 3: Deploy only through the existing dev-channel workflow**

Use the repository’s current bundle/update mechanism. Do not replace stable-channel nodes. Verify agent version and desktop SESSION_OPEN on one dev node before opening the Browser page.

Required checks:

~~~powershell
C:\Users\LABS\Desktop\XNC\bin\xnc.exe node list
C:\Users\LABS\Desktop\XNC\bin\xnc.exe audit
~~~

Expected: the selected dev node is online; audit contains desktop.open after starting the page.

- [ ] **Step 4: Run the HTML animation experiment in Chrome and Edge**

Use the existing tools/screendiag motion UI assets without rewriting them. For each browser, record a 30-second high-motion run and a 15-second static run.

Record:

- REST-to-first-render milliseconds;
- received, decoded, and rendered FPS;
- maximum decodeQueueSize;
- VIDEO_ACK interval distribution;
- keyframe count;
- agent CPU and working set;
- any REQUEST_KEYFRAME reason;
- whether closing the Browser returns ScreenStreamManager to idle.

Phase 1 acceptance:

- Chrome and Edge both establish CLIENT_HELLO and CODEC_CONFIG.
- first frame is decodable;
- VIDEO_ACK arrives every 500ms ±200ms while visible;
- wrong-generation and pre-key deltas never reach VideoDecoder;
- INPUT_* remains disabled;
- existing /screen and native rdp still work.

- [ ] **Step 5: Append evidence and commit**

Append an E5 subsection to docs/rdp-architecture-exploration.md with the exact node, browser versions, backend, test duration, measurements, deviations, and the decision for Phase 2. Do not change the architecture spec unless evidence disproves an approved requirement.

~~~powershell
git add docs/rdp-architecture-exploration.md
git commit -m "docs: record desktop protocol phase 1 results"
~~~

---

## Phase 1 Completion Gate

Phase 1 is complete only when:

- all Go and Web automated tests pass;
- Chrome and Edge complete the real-node video/ACK experiment;
- desktop remains view-only;
- existing screen and native rdp paths pass regression checks;
- E5 evidence is committed;
- no Rust Host or input code has been introduced.

After this gate, write the separate Phase 2 plan for Rust Host supervision, protected IPC, leases, and package integration. Phase 2 must preserve the Browser and Server protocol produced by this plan and replace DesktopPreviewHandler behind the Agent boundary.
