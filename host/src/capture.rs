//! 屏幕采集（Windows）：scrap DXGI → GDI 三层回退 + 帧比较跳帧 + 光标合成。
//!
//! - 回退/重试模式移植自 rustdesk `src/server/video_service.rs:805-864`
//!   （连续 WouldBlock>3 或采集错误 → set_gdi；显示器变化 → 重建）。
//! - 帧内容比较跳帧移植自 rustdesk `would_block_if_equal`（画面未变不编码）。
//! - 光标服务端合成（Sunshine 方案）：DXGI 不含光标，CPU 端 DrawIconEx 叠加；
//!   桌面无新帧仅光标移动时，基于上帧原始数据重新合成（Sunshine
//!   display_vram.cpp:1481-1505 的 CPU 版对应物）。

use std::io::{ErrorKind, Result as IoResult};
use std::time::{Duration, Instant};

use scrap::{Capturer, Display, Frame, TraitCapturer, TraitPixelBuffer};

/// DrawIconEx: DI_MASK|DI_IMAGE（winapi 0.3 未导出）
const DI_NORMAL: u32 = 0x0003;

/// 采集结果
pub enum CaptureOutcome<'a> {
    /// 可编码的 BGRA 帧（已含光标）
    Frame(&'a [u8]),
    /// 画面与光标均无变化（跳过编码）
    NoChange,
    /// 显示器拓扑/分辨率变化，需要重建采集器与编码器
    Reinit,
}

pub struct ScreenCapturer {
    cap: Capturer,
    pub width: usize,
    pub height: usize,
    /// 干净的上一帧原始数据（光标叠加的底图）
    last_raw: Vec<u8>,
    /// 叠加光标后的输出缓冲
    composited: Vec<u8>,
    would_block_since: Option<Instant>,
    display_check_at: Instant,
    cursor: CursorPainter,
    draw_cursor: bool,
    last_cursor_sig: (isize, i32, i32, bool),
    /// 强制产出一帧（viewer 新加入时静止桌面否则永远无帧：scrap GDI 内部
    /// would_block_if_equal 会把未变化帧变成 WouldBlock，IDR 请求无从消费）
    force_next: bool,
    // 统计
    pub frames_captured: u64,
    pub frames_skipped_equal: u64,
    pub cursor_only_frames: u64,
}

impl ScreenCapturer {
    pub fn new(display_index: usize, draw_cursor: bool) -> IoResult<Self> {
        let mut displays = Display::all()?;
        if displays.is_empty() {
            return Err(std::io::Error::new(ErrorKind::NotFound, "no display found"));
        }
        if display_index >= displays.len() {
            tracing::warn!(
                "display index {} out of range ({} displays), using primary",
                display_index, displays.len()
            );
        }
        // 优先用 primary（origin==0,0），否则按序号
        let idx = if display_index < displays.len() {
            display_index
        } else {
            displays
                .iter()
                .position(|d| d.is_primary())
                .unwrap_or(0)
        };
        let display = displays.swap_remove(idx);
        let width = display.width();
        let height = display.height();
        let disp_name = display.name();
        tracing::info!(
            display_index = idx,
            name = %disp_name,
            width, height,
            "creating DXGI capturer"
        );
        let cap = Capturer::new(display)?; // 失败时内部自动可用 GDI 显示器（scrap 保证）
        Ok(Self {
            cap,
            width,
            height,
            last_raw: Vec::new(),
            composited: Vec::new(),
            would_block_since: None,
            display_check_at: Instant::now(),
            cursor: CursorPainter::new(),
            draw_cursor,
            last_cursor_sig: (0, 0, 0, false),
            force_next: false,
            frames_captured: 0,
            frames_skipped_equal: 0,
            cursor_only_frames: 0,
        })
    }

    /// 强制下一 tick 产出一帧（即使画面与光标均未变化）。
    /// 用于新 viewer 加入：服务器合成 frameLoss 置 IDR 标志，但静止桌面下
    /// 采集端永远 NoChange，编码路径不执行，viewer 将等不到关键帧。
    pub fn force_frame(&mut self) {
        self.force_next = true;
    }

    pub fn is_gdi(&self) -> bool {
        self.cap.is_gdi()
    }

    /// 拉取一帧。timeout 为 DXGI AcquireNextFrame 等待时长。
    pub fn next(&mut self, timeout: Duration) -> IoResult<CaptureOutcome<'_>> {
        // 显示器拓扑巡检（每 1s，rustdesk try_broadcast_display_changed 思路）
        if self.display_check_at.elapsed() >= Duration::from_secs(1) {
            self.display_check_at = Instant::now();
            if let Ok(displays) = Display::all() {
                let geometry: Vec<(i32, i32, usize, usize)> = displays
                    .iter()
                    .map(|d| {
                        let (x, y) = d.origin();
                        (x, y, d.width(), d.height())
                    })
                    .collect();
                let cur = (self.width, self.height);
                if !geometry.iter().any(|&(_, _, w, h)| (w, h) == cur) {
                    tracing::warn!("display geometry changed: {:?}, reinit", geometry);
                    return Ok(CaptureOutcome::Reinit);
                }
            }
        }

        // 拉帧（WouldBlock = 无新帧）
        let frame = match self.cap.frame(timeout) {
            Ok(f) => f,
            Err(e) if e.kind() == ErrorKind::WouldBlock => {
                // 静止桌面 DXGI 合法地不产生帧；仅当连续 2s 无帧才视为采集异常
                // → 切 GDI（rustdesk "No image, fall back to gdi" 的时间量纲修正版）
                let since = *self.would_block_since.get_or_insert_with(Instant::now);
                if since.elapsed() > Duration::from_secs(2) && !self.cap.is_gdi() {
                    tracing::warn!("no DXGI frames for 2s, falling back to GDI");
                    if self.cap.set_gdi() {
                        tracing::info!("GDI capture enabled");
                    }
                }
                return self.cursor_only_or_none();
            }
            Err(e) => {
                // 运行中出错 → 尝试切 GDI 再抛给上层重建（rustdesk: "dxgi error, fall back to gdi"）
                if !self.cap.is_gdi() {
                    let _ = self.cap.set_gdi();
                    tracing::warn!(?e, "capture error, switched to GDI, retry next tick");
                    return Ok(CaptureOutcome::NoChange);
                }
                return Err(e);
            }
        };

        let Frame::PixelBuffer(pb) = &frame else {
            return self.cursor_only_or_none();
        };
        let (data, w, h) = (pb.data(), pb.width(), pb.height());
        if data.is_empty() {
            return self.cursor_only_or_none();
        }
        self.would_block_since = None;
        self.frames_captured += 1;

        // 分辨率/步长变化 → 重建（首帧时 last_raw 为空属正常，不算变化）
        let stride_changed = !self.last_raw.is_empty() && data.len() != self.last_raw.len();
        if w != self.width || h != self.height || stride_changed {
            tracing::warn!(w, h, expect_w = self.width, expect_h = self.height, data_len = data.len(), "resolution changed, reinit");
            return Ok(CaptureOutcome::Reinit);
        }

        // 帧内容比较跳帧（rustdesk would_block_if_equal）
        let changed = data != self.last_raw.as_slice();
        let cursor_sig = self.cursor.signature();
        let cursor_moved = cursor_sig != self.last_cursor_sig;

        if !changed && !cursor_moved {
            self.frames_skipped_equal += 1;
            return Ok(CaptureOutcome::NoChange);
        }
        self.last_cursor_sig = cursor_sig;

        if changed {
            self.last_raw.clear();
            self.last_raw.extend_from_slice(data);
        } else {
            self.cursor_only_frames += 1;
        }

        // 合成输出：干净底图 + 光标
        self.composited.clear();
        self.composited.extend_from_slice(&self.last_raw);
        if self.draw_cursor {
            self.cursor.draw(&mut self.composited, w, h);
        }
        Ok(CaptureOutcome::Frame(&self.composited))
    }

    /// 桌面无新帧时：仅光标移动则重新合成（Sunshine cursor-only 路径）；
    /// force_next 时无条件产出一帧（新 viewer 需要首帧/关键帧）。
    fn cursor_only_or_none(&mut self) -> IoResult<CaptureOutcome<'_>> {
        let force = self.force_next && !self.last_raw.is_empty();
        if force {
            self.force_next = false;
            self.cursor_only_frames += 1;
        } else if !self.draw_cursor || self.last_raw.is_empty() {
            return Ok(CaptureOutcome::NoChange);
        } else {
            let sig = self.cursor.signature();
            if sig == self.last_cursor_sig {
                return Ok(CaptureOutcome::NoChange);
            }
            self.last_cursor_sig = sig;
            self.cursor_only_frames += 1;
        }
        self.composited.clear();
        self.composited.extend_from_slice(&self.last_raw);
        if self.draw_cursor {
            self.cursor.draw(&mut self.composited, self.width, self.height);
        }
        Ok(CaptureOutcome::Frame(&self.composited))
    }
}

// ---------------- 光标合成（winapi） ----------------

/// (hCursor, x, y, visible)
struct CursorPainter {
    initialized: bool,
}

impl CursorPainter {
    fn new() -> Self {
        Self { initialized: false }
    }

    fn signature(&self) -> (isize, i32, i32, bool) {
        unsafe {
            let mut ci: winapi::um::winuser::CURSORINFO = std::mem::zeroed();
            ci.cbSize = std::mem::size_of::<winapi::um::winuser::CURSORINFO>() as u32;
            if winapi::um::winuser::GetCursorInfo(&mut ci) == 0 {
                return (0, 0, 0, false);
            }
            let visible = ci.flags & winapi::um::winuser::CURSOR_SHOWING != 0;
            (ci.hCursor as isize, ci.ptScreenPos.x, ci.ptScreenPos.y, visible)
        }
    }

    /// 在 BGRA 帧上叠加当前光标（失败静默跳过，仅告警一次）。
    fn draw(&mut self, bgra: &mut [u8], w: usize, h: usize) {
        unsafe {
            let mut ci: winapi::um::winuser::CURSORINFO = std::mem::zeroed();
            ci.cbSize = std::mem::size_of::<winapi::um::winuser::CURSORINFO>() as u32;
            if winapi::um::winuser::GetCursorInfo(&mut ci) == 0
                || ci.flags & winapi::um::winuser::CURSOR_SHOWING == 0
                || ci.hCursor.is_null()
            {
                return;
            }
            let mut ii: winapi::um::winuser::ICONINFO = std::mem::zeroed();
            if winapi::um::winuser::GetIconInfo(ci.hCursor, &mut ii) == 0 {
                self.warn_once("GetIconInfo failed");
                return;
            }
            // 释放 GetIconInfo 返回的位图
            let free_bmp = |h: winapi::shared::windef::HBITMAP| {
                if !h.is_null() {
                    winapi::um::wingdi::DeleteObject(h as *mut winapi::ctypes::c_void);
                }
            };

            let hdc_screen = winapi::um::winuser::GetDC(std::ptr::null_mut());
            let hdc_mem = winapi::um::wingdi::CreateCompatibleDC(hdc_screen);
            if hdc_mem.is_null() {
                self.warn_once("CreateCompatibleDC failed");
                free_bmp(ii.hbmMask);
                free_bmp(ii.hbmColor);
                winapi::um::winuser::ReleaseDC(std::ptr::null_mut(), hdc_screen);
                return;
            }

            // 把帧缓冲包成 top-down 32bpp DIB
            let bi = winapi::um::wingdi::BITMAPINFO {
                bmiHeader: winapi::um::wingdi::BITMAPINFOHEADER {
                    biSize: std::mem::size_of::<winapi::um::wingdi::BITMAPINFOHEADER>() as u32,
                    biWidth: w as i32,
                    biHeight: -(h as i32), // 负高 = top-down
                    biPlanes: 1,
                    biBitCount: 32,
                    biCompression: winapi::um::wingdi::BI_RGB,
                    ..std::mem::zeroed()
                },
                ..std::mem::zeroed()
            };
            let mut bits_ptr: *mut winapi::shared::ntdef::VOID = std::ptr::null_mut();
            let dib = winapi::um::wingdi::CreateDIBSection(
                hdc_mem, &bi, winapi::um::wingdi::DIB_RGB_COLORS, &mut bits_ptr,
                std::ptr::null_mut(), 0,
            );
            if dib.is_null() || bits_ptr.is_null() {
                self.warn_once("CreateDIBSection failed");
                winapi::um::wingdi::DeleteDC(hdc_mem);
                free_bmp(ii.hbmMask);
                free_bmp(ii.hbmColor);
                winapi::um::winuser::ReleaseDC(std::ptr::null_mut(), hdc_screen);
                return;
            }
            let old = winapi::um::wingdi::SelectObject(hdc_mem, dib as *mut winapi::ctypes::c_void);
            let buf_len = w * h * 4;
            std::ptr::copy_nonoverlapping(bgra.as_ptr(), bits_ptr as *mut u8, buf_len.min(bgra.len()));

            // 画光标（热点偏移；位置 clamp 到帧内）
            let x = (ci.ptScreenPos.x - ii.xHotspot as i32).clamp(0, w as i32 - 1);
            let y = (ci.ptScreenPos.y - ii.yHotspot as i32).clamp(0, h as i32 - 1);
            winapi::um::winuser::DrawIconEx(
                hdc_mem, x, y, ci.hCursor, 0, 0, 0,
                std::ptr::null_mut(),
                DI_NORMAL,
            );

            std::ptr::copy_nonoverlapping(bits_ptr as *const u8, bgra.as_mut_ptr(), buf_len.min(bgra.len()));

            winapi::um::wingdi::SelectObject(hdc_mem, old);
            winapi::um::wingdi::DeleteObject(dib as *mut winapi::ctypes::c_void);
            winapi::um::wingdi::DeleteDC(hdc_mem);
            free_bmp(ii.hbmMask);
            free_bmp(ii.hbmColor);
            winapi::um::winuser::ReleaseDC(std::ptr::null_mut(), hdc_screen);
        }
    }

    fn warn_once(&mut self, msg: &str) {
        if !self.initialized {
            tracing::warn!(msg, "cursor compose failed (disabled logging for repeats)");
            self.initialized = true;
        }
    }
}
