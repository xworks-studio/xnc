/** REST API DTOs (snake_case matches the Go server's JSON encoding). */

export interface UserDTO {
  id: string;
  email: string;
  display_name: string;
  /** 仅 admin 视角端点（listUsers/updateUser）携带。 */
  is_admin?: boolean;
}

export interface NodeDTO {
  id: string;
  name: string;
  cluster: string;
  hostname: string;
  os_version: string;
  agent_version: string;
  shell_type: string;
  status: "online" | "offline" | "disabled";
  last_seen_at: string | null;
}

export interface ClusterDTO {
  id: string;
  name: string;
  /** 0005 起：系统自动建的个人默认 cluster（徽标用）。 */
  personal?: boolean;
  /** 0005 起：我在该 cluster 的角色（owner 时页内开放管理操作）。 */
  role?: "owner" | "operator" | "viewer";
}

/** GET /api/clusters/{id}/members rows. */
export interface MemberDTO {
  user_id: string;
  email: string;
  display_name: string;
  role: "owner" | "operator" | "viewer";
}

/** POST /api/auth/login response. */
export interface LoginResponse {
  token: string;
  user: UserDTO;
}

/**
 * POST /api/nodes/{id}/desktop 202 响应的 relay 候选（relay-plane 新增；
 * 字段 camelCase 与会话响应其余字段一致）。同一 relay 的传输变体，有序
 * （wt 主路在前，ws 兜底在后）；certSha256 仅纯 IP 自签模式的 relay 携带
 * ——64 位 hex = relay 自签证书 DER 的 SHA-256，浏览器 WebTransport
 * serverCertificateHashes 钉扎用；缺失 = 标准 Web PKI（不传该选项）。
 * 旧形态服务器不带 candidates 字段，客户端回退 wtUrl/wsUrl 既有逻辑。
 */
export interface DesktopCandidate {
  transport: "wt" | "ws";
  host: string;
  port: number;
  path: string;
  certSha256?: string;
  relayId?: string; // 归属中继（"rl-0" = 主站内嵌）
  region?: string; // 区域标签（embedded = 主站内嵌）
  displayHost?: string; // relay 域名（server 取其 sdata 端点标注；仅展示用，连接仍走 host 裸 IP）
}
