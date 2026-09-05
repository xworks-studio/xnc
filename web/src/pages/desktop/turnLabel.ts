/**
 * TURN 中继路径标签（设计 §3.3）：selected candidate-pair 的 local
 * candidate 为 relay 时，其 address 即 TURN 服务器地址（TURN 在该地址上
 * 为本端分配 relay 端口）。纯函数，供 DesktopLive 的 stats 轮询调用。
 */
export interface LocalCandLike {
  candidateType?: string;
  address?: string;
  relayProtocol?: string;
}

export function turnRelayLabel(local: LocalCandLike | undefined): string {
  if (!local) return "—";
  if (local.candidateType === "relay") {
    const proto = local.relayProtocol ?? "udp";
    return local.address ? `via TURN ${local.address} (${proto})` : "via TURN (?)";
  }
  return "direct";
}
