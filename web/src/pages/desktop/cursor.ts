/**
 * Cursor overlay geometry (M1-Slice3 Task 4).
 *
 * The cursor DataChannel carries `[s32 x][s32 y][u8 visible]` with x/y in
 * the HOST_HELLO stream logical-pixel space (native MapCursorToStream is
 * the exact inverse of MapMoveToAbs, so viewer input and the echoed dot
 * share one coordinate space). The <video> renders with
 * object-fit:contain, so both directions need the same letterbox map
 * between stream space and the element's content box:
 *
 *   element px = offset + stream px * scale,  scale = min(ew/sw, eh/sh)
 *
 * The dot is sized by the same scale (constant footprint in stream space,
 * clamped so it never disappears on tiny windows or balloons on 4K).
 */

import type { CSSProperties } from "react";

export interface StreamMapping {
  /** Content-box origin inside the element (letterbox bars). */
  offsetX: number;
  offsetY: number;
  /** element px per stream px. */
  scale: number;
}

export interface StreamDims {
  w: number;
  h: number;
}

/** Letterbox map of the video content box inside `el` for the given
 * stream dims; null while either side is unknown (no frames yet). */
export function streamMapping(
  el: HTMLElement | null,
  dims: StreamDims | null,
): StreamMapping | null {
  if (!el || !dims || dims.w <= 0 || dims.h <= 0) return null;
  const rect = el.getBoundingClientRect();
  if (rect.width <= 0 || rect.height <= 0) return null;
  const scale = Math.min(rect.width / dims.w, rect.height / dims.h);
  return {
    offsetX: (rect.width - dims.w * scale) / 2,
    offsetY: (rect.height - dims.h * scale) / 2,
    scale,
  };
}

/** Style for the absolutely-positioned dot inside the video container
 * (translate(-50%,-50%) centers it on the cursor point). */
export function cursorDotStyle(
  m: StreamMapping,
  x: number,
  y: number,
): CSSProperties {
  const d = Math.min(40, Math.max(5, 12 * m.scale));
  return {
    left: m.offsetX + x * m.scale,
    top: m.offsetY + y * m.scale,
    width: d,
    height: d,
  };
}

/** Decode a cursor-channel frame `[s32 x][s32 y][u8 visible]` (9B LE);
 * anything else (wrong size / non-binary) is dropped by the caller. */
export interface CursorUpdate {
  x: number;
  y: number;
  visible: boolean;
}

export function decodeCursor(buf: ArrayBuffer): CursorUpdate | null {
  if (buf.byteLength !== 9) return null;
  const v = new DataView(buf);
  return {
    x: v.getInt32(0, true),
    y: v.getInt32(4, true),
    visible: v.getUint8(8) !== 0,
  };
}
