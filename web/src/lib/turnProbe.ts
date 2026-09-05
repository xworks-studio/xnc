/**
 * 浏览器→TURN 回环探测（设计 §3.2）：两条 RTCPeerConnection 经同一 TURN
 * relay 互联，取 selected candidate-pair currentRoundTripTime/2 为单向
 * 时延。RTCPeerConnection 经 pcFactory 注入（happy-dom 无实现，测试替身）。
 */

export interface ProbeTarget {
  urls: string[];
  username: string;
  credential: string;
}

export type PCFactory = (cfg: RTCConfiguration) => RTCPeerConnection;

/** 可注入工厂（测试替身；生产 = window.RTCPeerConnection）。 */
export const pcFactory: { make: PCFactory } = {
  make: (cfg) => new RTCPeerConnection(cfg),
};

const DEFAULT_TIMEOUT_MS = 8000;

/**
 * probeTurn — 返回浏览器→TURN 单向时延（ms，0.1 精度）。
 * 失败：超时 reject Error("timeout")；连通但无 rtt 统计 reject
 * Error("no rtt")。调用方负责串行（并发探测会互相抬时延）。
 */
export async function probeTurn(target: ProbeTarget, timeoutMs = DEFAULT_TIMEOUT_MS): Promise<number> {
  const mk = () =>
    pcFactory.make({
      iceServers: [
        { urls: target.urls, username: target.username, credential: target.credential },
      ],
      iceTransportPolicy: "relay",
    });
  const a = mk();
  const b = mk();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const cleanup = () => {
    if (timer !== undefined) clearTimeout(timer);
    try {
      a.close();
    } catch {
      /* already closed */
    }
    try {
      b.close();
    } catch {
      /* already closed */
    }
  };
  try {
    return await new Promise<number>((resolve, reject) => {
      timer = setTimeout(() => reject(new Error("timeout")), timeoutMs);
      a.onicecandidate = (e) => {
        if (e.candidate) void b.addIceCandidate(e.candidate).catch(() => {});
      };
      b.onicecandidate = (e) => {
        if (e.candidate) void a.addIceCandidate(e.candidate).catch(() => {});
      };
      const ch = a.createDataChannel("probe");
      // 信道开 = ICE（经 TURN relay）已连通；此后轮询 getStats 等
      // currentRoundTripTime 出现（Chrome 需一次 STUN consent 后才填）。
      ch.onopen = async () => {
        for (let i = 0; i < 6; i++) {
          let rtt = 0;
          try {
            const report = await a.getStats();
            report.forEach((s) => {
              const st = s as { type?: string; nominated?: boolean; currentRoundTripTime?: number };
              if (st.type === "candidate-pair" && typeof st.currentRoundTripTime === "number") {
                if (st.nominated || rtt === 0) rtt = st.currentRoundTripTime;
              }
            });
          } catch {
            /* pc closing */
          }
          if (rtt > 0) {
            // 往返/2 = 浏览器→TURN 单程（回环两腿同路径）。
            resolve(Math.round((rtt * 1000 * 10) / 2) / 10);
            return;
          }
          await new Promise((r) => setTimeout(r, 500));
        }
        reject(new Error("no rtt"));
      };
      void (async () => {
        try {
          const offer = await a.createOffer();
          await a.setLocalDescription(offer);
          await b.setRemoteDescription(offer);
          const answer = await b.createAnswer();
          await b.setLocalDescription(answer);
          await a.setRemoteDescription(answer);
        } catch (e) {
          reject(e instanceof Error ? e : new Error(String(e)));
        }
      })();
    });
  } finally {
    cleanup();
  }
}
