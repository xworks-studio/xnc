// @vitest-environment happy-dom
/**
 * turnProbe 单测：假 RTCPeerConnection 驱动回环协商；断言 RTT/2 取整到
 * 0.1ms 与超时拒绝。假实现只覆盖 probeTurn 触碰的面。
 */
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { probeTurn, pcFactory } from "./turnProbe";
import type { PCFactory } from "./turnProbe";

/** 最小假 PC：两实例经模块级信箱交换 SDP/候选；getStats 返回固定 pair。 */
function makeFakePC(rttSeconds: number) {
  const mailboxes: Array<(msg: unknown) => void> = [];
  const mk = () => {
    const pc: Record<string, unknown> = {
      onicecandidate: null,
      createDataChannel: vi.fn(() => ({ onopen: null as null | (() => void) })),
      createOffer: vi.fn(async () => ({ type: "offer", sdp: "o" })),
      createAnswer: vi.fn(async () => ({ type: "answer", sdp: "a" })),
      setLocalDescription: vi.fn(async () => {
        // 本地描述就绪 = 产出候选（假 relay 候选）。
        const cb = pc.onicecandidate as ((e: { candidate: {} }) => void) | null;
        cb?.({ candidate: {} });
      }),
      setRemoteDescription: vi.fn(async () => {}),
      addIceCandidate: vi.fn(async () => {}),
      getStats: vi.fn(async () => [
        { type: "candidate-pair", nominated: true, currentRoundTripTime: rttSeconds },
      ]),
      close: vi.fn(),
    };
    return pc as unknown as RTCPeerConnection & { createDataChannel: ReturnType<typeof vi.fn> };
  };
  const a = mk();
  const b = mk();
  // 互通：A 的候选直接喂 B（假实现无需真实 ICE）。
  const origA = a.onicecandidate;
  void origA;
  return { a, b, mailboxes };
}

describe("probeTurn", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("resolves with rtt/2 in ms (1 decimal)", async () => {
    const { a, b } = makeFakePC(0.041); // 41ms RTT → 20.5ms 单向
    let n = 0;
    const factory: PCFactory = () => (n++ === 0 ? a : b);
    pcFactory.make = factory;
    const p = probeTurn({ urls: ["turn:1.2.3.4:3478?transport=udp"], username: "u", credential: "p" });
    // 假 PC 的 datachannel onopen 由 setLocalDescription 链路外触发——
    // 直接在微任务后手动开信道。
    await vi.advanceTimersByTimeAsync(0);
    const ch = a.createDataChannel.mock.results[0]?.value as { onopen: null | (() => void) };
    ch.onopen?.();
    await expect(p).resolves.toBe(20.5);
  });

  it("rejects with timeout after timeoutMs", async () => {
    const { a, b } = makeFakePC(1);
    let n = 0;
    pcFactory.make = () => (n++ === 0 ? a : b);
    const p = probeTurn({ urls: ["turn:1.2.3.4:3478?transport=udp"], username: "u", credential: "p" }, 50);
    // 假定时器不会自行推进：先挂断言（reject 处理器提前就位，避免
    // unhandled rejection），再推进时间触发 50ms 定时器，最后 await。
    const assertion = expect(p).rejects.toThrow("timeout");
    await vi.advanceTimersByTimeAsync(100);
    await assertion;
  });
});
