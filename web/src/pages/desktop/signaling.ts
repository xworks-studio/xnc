/**
 * Desktop-live signaling vocabulary (agent/desktop/session.go contract):
 * the frame interface, state/lease/SAS code notices, and the stream-dims
 * type shared by the DesktopLive page. Pure data — no React imports.
 *
 * Extracted from DesktopLive.tsx (2026-08-24 prod retro follow-up, Task 7);
 * zero behavior change — the component imports from here.
 */

import type { StreamDims } from "./cursor";

export type { StreamDims };

export interface DisplayEntry {
  index: number;
  originX: number;
  originY: number;
  w: number;
  h: number;
  primary: boolean;
}

export interface SignalingFrame {
  type: string;
  sdp?: string;
  candidate?: RTCIceCandidateInit | null;
  code?: string;
  message?: string;
  width?: number;
  height?: number;
  fps?: number;
  /** M2-S3 Task 5: ready-frame displays table */
  displays?: DisplayEntry[];
  /** display_changed generation */
  generation?: number;
  /** display_changed geometry */
  w?: number;
  h?: number;
  /** display_changed reset reason / lease_denied / lease_revoked */
  reason?: string;
  /** lease_granted */
  leaseId?: string;
  /** secure_attention_result */
  ok?: boolean;
  hr?: number;
}

export const LEASE_NOTICES: Record<string, string> = {
  held: "input lease held by another viewer — retry later",
  idle: "input lease revoked: 30s without input",
  disconnect: "input lease revoked",
};

/** M2-Slice1 Task 2/3 desktop STATE codes worth a toast (uniform with the
 * display_changed notice; the code also stays in the status bar). */
export const STATE_NOTICES: Record<string, string> = {
  recovering:
    "安全桌面已激活(UAC/锁屏/系统弹窗)——画面暂停,输入仍可用:可盲按 Alt+Y 批准 UAC 或 Ctrl+Alt+Del;弹窗关闭后自动恢复",
  capture_rebuilt: "capture rebuilt — stream restored",
  backend_changed: "capture backend changed (DXGI/GDI ladder)",
};

/** secure_attention_result stable codes worth more than a generic line. */
export const SAS_NOTICES: Record<string, string> = {
  SAS_DENIED: "secure attention denied by the host core (--allow-sas gate)",
  SAS_UNAVAILABLE: "secure attention unavailable on the host (sas.dll)",
  busy: "secure attention already in flight on this session",
  unsupported: "secure attention unsupported by this agent",
  core_unavailable: "secure attention failed: agent cannot reach xnc-core",
  core_error: "secure attention failed: xnc-core error",
};
