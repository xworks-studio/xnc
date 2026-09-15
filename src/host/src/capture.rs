//! 屏幕采集（Windows）：scrap DXGI → GDI 回退 + 帧比较跳帧 + 可选光标合成。
//!
//! - GDI 回退分三层（2026-09-15 规范，决策见 fallback_action）：
//!   ①首帧探针（1.5s）：duplication 创建/回切后首帧必达，超期 = DXGI
//!     不可用（盒盖/屏休眠）→ 立即 GDI，冷启动首帧从 30s 计时器等死
//!     变为亚秒级判定；②产帧需求兜底（5s）：force_frame 挂起无产出
//!     （会话中途屏入睡/静止桌面 IDR 饿死）→ GDI；③稳态 30s：原语义
//!     保留（健康静止桌面不误降级）。GDI 驻留期每 60s/输入事件回探
//!     DXGI（cancel_gdi + 探针裁决）。
//! - 回退/重试模式移植自 rustdesk `src/server/video_service.rs:805-864`
//!   （WouldBlock 持续超阈值或采集错误 → set_gdi；显示器变化 → 重建）。
//! - 帧内容比较跳帧移植自 rustdesk `would_block_if_equal`（画面未变不编码）。
//! - 光标模型（2026-09-09 定稿）：**默认不合成进视频流**——web 端以本地
//!   十字准星（浏览器原生渲染，零延迟）指示指针位置，流内光标只带来
//!   光标移动逐帧重编码的 churn 与 GDI 模式 CAPTUREBLT 烤入光标的
//!   双影/闪烁。`--cursor` 调试开关可恢复服务端合成（Sunshine 方案：
//!   桌面无新帧仅光标移动时基于上帧原始数据重新合成）。
//! - 合成路径的防闪烁实现（供 --cursor 调试形态使用）：
//!   1) 判变与绘制共用一次 GetCursorInfo 采样（两次独立采样会被高频
//!      SetCursorPos 注入插在中间，绘制位与判定位错帧 → 抖动）；
//!   2) hCursor → 预乘 BGRA 精灵缓存（GetIconInfo+GetDIBits 一次转换，
//!      逐帧手工混合），构建失败回退 DrawIconEx 旧路径；
//!   3) ptScreenPos 为虚拟屏绝对坐标，绘制/注入必须减加被采集显示器
//!      原点（副屏错位修复）。

use std::io::{ErrorKind, Result as IoResult};
use std::sync::Arc;
use std::time::{Duration, Instant};

use scrap::{Capturer, Display, Frame, TraitCapturer, TraitPixelBuffer};

/// DrawIconEx: DI_MASK|DI_IMAGE（winapi 0.3 未导出）
const DI_NORMAL: u32 = 0x0003;

/// 光标精灵缓存容量（系统光标个位数；超过即 LRU 淘汰）。
const CURSOR_CACHE_CAP: usize = 8;
/// 精灵尺寸防御上限（正常光标 ≤64px；掩码位图高度为 2 倍）。
const SPRITE_MAX_DIM: usize = 512;
/// 首帧探针窗口：DXGI duplication 创建（或 GDI 回切）后首帧必达（静止
/// 桌面亦然——所有 duplication 采集器依赖此语义取基础帧）。超期即判定
/// DXGI 不可用（盒盖/屏休眠典型：首帧永远不来）→ 立即降级 GDI。
const DXGI_PROBE_TIMEOUT: Duration = Duration::from_millis(1500);
/// 产帧需求兜底时限（产品口径 5s）：force_frame 挂起（新 viewer 待 IDR /
/// frameLoss 重传）超过此时限仍无产出 → 降级 GDI。覆盖探针之后的窗口
/// （会话中途显示器入睡、纯 IDR 请求在静止桌面饿死等）。
const FRAME_DEMAND_TIMEOUT: Duration = Duration::from_secs(5);
/// GDI 驻留期 DXGI 回探间隔：静默期 cancel_gdi 试切（输入事件可提前
/// 触发，见 input::take_dxgi_retry_hint）。试切对静态画面无感知，失败
/// 由首帧探针在 1.5s 内再次降级。
const DXGI_RETRY_INTERVAL: Duration = Duration::from_secs(60);

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
    /// 被采集显示器在虚拟屏中的原点（ptScreenPos/SetCursorPos 均为虚拟屏
    /// 绝对坐标，绘制与注入需减/加该值）
    pub origin: (i32, i32),
    /// 干净的上一帧原始数据（光标叠加的底图）
    last_raw: Vec<u8>,
    /// 叠加光标后的输出缓冲
    composited: Vec<u8>,
    would_block_since: Option<Instant>,
    /// 首帧探针期限（Some = 探针在途）：DXGI 路由下任何 Ok 帧（含纯
    /// 光标帧）都会证明后端活着并收口探针；WouldBlock 持续到期限即
    /// ProbeFail。fall_to_gdi / 构造期落 GDI 时置 None。
    dxgi_probe_deadline: Option<Instant>,
    /// 产帧需求挂起时刻（force_frame 置位、最早时刻保留；成功产出
    /// Frame 或降级 GDI 时清除）。
    frame_demand_since: Option<Instant>,
    /// GDI 驻留期的 DXGI 回探时刻（静默期 WouldBlock 分支消费；输入
    /// 事件经 retry_dxgi_now 提前）。None = 非 GDI 或无回探安排。
    dxgi_retry_at: Option<Instant>,
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
        let origin = display.origin();
        let disp_name = display.name();
        tracing::info!(
            display_index = idx,
            name = %disp_name,
            width, height,
            ?origin,
            "creating DXGI capturer"
        );
        let cap = Capturer::new(display)?; // 失败时内部自动可用 GDI 显示器（scrap 保证）
        // 探针/回探的初始安排：DXGI 起步 → 首帧探针在途；构造期即落 GDI
        // （duplication 创建失败）→ 直接安排回探。
        let boot = Instant::now();
        let (dxgi_probe_deadline, dxgi_retry_at) = if cap.is_gdi() {
            (None, Some(boot + DXGI_RETRY_INTERVAL))
        } else {
            (Some(boot + DXGI_PROBE_TIMEOUT), None)
        };
        Ok(Self {
            cap,
            width,
            height,
            origin,
            last_raw: Vec::new(),
            composited: Vec::new(),
            would_block_since: None,
            dxgi_probe_deadline,
            frame_demand_since: None,
            dxgi_retry_at,
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
    /// 同时挂起产帧需求计时：DXGI 给不出（盒盖/屏休眠）超过
    /// FRAME_DEMAND_TIMEOUT 时降级 GDI 兜底。
    pub fn force_frame(&mut self) {
        self.force_next = true;
        // 取最早挂起时刻：viewer 加入与 IDR 请求叠加时不应刷新兜底窗口。
        if self.frame_demand_since.is_none() {
            self.frame_demand_since = Some(Instant::now());
        }
    }

    pub fn is_gdi(&self) -> bool {
        self.cap.is_gdi()
    }

    /// 拉取一帧。timeout 为 DXGI AcquireNextFrame 等待时长。
    pub fn next(&mut self, timeout: Duration) -> IoResult<CaptureOutcome<'_>> {
        // 显示器拓扑巡检（每 500ms，rustdesk try_broadcast_display_changed 思路；
        // 1s → 500ms：盒盖过渡时虚拟屏上线的识别延迟减半）
        if self.display_check_at.elapsed() >= Duration::from_millis(500) {
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
                // 分层兜底（决策纯函数见 fallback_action，单测覆盖分层语义）：
                // ①首帧探针：duplication 首帧必达，超期 = DXGI 不可用；
                // ②产帧需求：force_frame 挂起 5s 无产出（会话中途屏入睡等）；
                // ③GDI 驻留回探：静默期试切回 DXGI（输入事件提前触发）。
                let now = Instant::now();
                match fallback_action(
                    self.cap.is_gdi(),
                    self.dxgi_probe_deadline,
                    self.frame_demand_since,
                    self.dxgi_retry_at,
                    now,
                ) {
                    FallbackAction::ProbeFail => {
                        tracing::warn!(
                            timeout_ms = DXGI_PROBE_TIMEOUT.as_millis() as u64,
                            "DXGI delivered no initial frame within probe window, falling back to GDI"
                        );
                        self.fall_to_gdi(now);
                    }
                    FallbackAction::DemandTimeout => {
                        tracing::warn!(
                            timeout_ms = FRAME_DEMAND_TIMEOUT.as_millis() as u64,
                            "DXGI unresponsive with pending frame demand, falling back to GDI"
                        );
                        self.fall_to_gdi(now);
                    }
                    FallbackAction::RetryDue => {
                        tracing::info!("GDI dwell reached retry deadline, probing DXGI");
                        self.try_dxgi_again(now);
                    }
                    FallbackAction::None => {}
                }
                // 静止桌面 DXGI 合法地不产生帧（本地光标模型下静止桌面
                // 本就该零帧）。仅当连续 30s 无帧才视为 DXGI 采集异常 →
                // 切 GDI（rustdesk "No image, fall back to gdi" 的量纲修正：
                // 原承 2s 会在每次桌面静止 2s 后误降级——GDI 全屏 BitBlt
                // 更慢，且 CAPTUREBLT 会把闪烁的物理光标烤进帧里，真机
                // 表现为画面卡顿；30s 兼顾死 DXGI 的恢复时限）
                let since = *self.would_block_since.get_or_insert_with(Instant::now);
                if since.elapsed() > Duration::from_secs(30) && !self.cap.is_gdi() {
                    tracing::warn!("no DXGI frames for 30s, falling back to GDI");
                    self.fall_to_gdi(now);
                }
                return self.cursor_only_or_none();
            }
            // 显示器消失/会话断开（盒盖面板消亡的典型错误）：GDI 同样救
            // 不了——跳过 GDI 掩盖直接上抛，pipeline 立即重建并重枚举
            // （新出现的 IDD 虚拟屏会被采上，缩短合盖过渡黑屏窗口）。
            Err(e)
                if e.kind() == ErrorKind::ConnectionReset
                    || e.kind() == ErrorKind::ConnectionAborted =>
            {
                tracing::warn!(?e, "capture lost, reinit immediately");
                return Err(e);
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

        // 任何 Ok（含纯光标帧）都证明当前后端活着：首帧探针收口。
        self.dxgi_probe_deadline = None;

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

        // 帧内容比较跳帧（rustdesk would_block_if_equal）。
        // 光标单次采样：判变与稍后绘制共用同一 snap（两次独立 GetCursorInfo
        // 会被高频注入插队，绘制位与判定位错帧）。
        let changed = data != self.last_raw.as_slice();
        let snap = self.cursor.snapshot();
        let cursor_sig = CursorPainter::sig(&snap);
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
            self.cursor.draw(&mut self.composited, w, h, &snap, self.origin);
        }
        // 帧已实际产出：产帧需求（新 viewer/IDR 待承载）视为满足。
        self.frame_demand_since = None;
        Ok(CaptureOutcome::Frame(&self.composited))
    }

    /// 桌面无新帧时：仅光标移动则重新合成（Sunshine cursor-only 路径）；
    /// force_next 时无条件产出一帧（新 viewer 需要首帧/关键帧）。
    fn cursor_only_or_none(&mut self) -> IoResult<CaptureOutcome<'_>> {
        let force = self.force_next && !self.last_raw.is_empty();
        let mut snap_opt: Option<CursorSnap> = None;
        if force {
            self.force_next = false;
            self.cursor_only_frames += 1;
        } else if !self.draw_cursor || self.last_raw.is_empty() {
            return Ok(CaptureOutcome::NoChange);
        } else {
            let snap = self.cursor.snapshot();
            if CursorPainter::sig(&snap) == self.last_cursor_sig {
                return Ok(CaptureOutcome::NoChange);
            }
            self.last_cursor_sig = CursorPainter::sig(&snap);
            self.cursor_only_frames += 1;
            snap_opt = Some(snap);
        }
        // force 路径没有采样过：补一次（首帧场景光标未必在屏幕上）
        let snap = match snap_opt {
            Some(s) => s,
            None => self.cursor.snapshot(),
        };
        self.composited.clear();
        self.composited.extend_from_slice(&self.last_raw);
        if self.draw_cursor {
            self.cursor.draw(&mut self.composited, self.width, self.height, &snap, self.origin);
        }
        // 帧已实际产出：产帧需求视为满足。
        self.frame_demand_since = None;
        Ok(CaptureOutcome::Frame(&self.composited))
    }

    /// 降级 GDI：收口探针/需求状态，安排 DXGI 回探；30s 稳态窗口与
    /// 探针窗口解耦（各自重新起算）。
    fn fall_to_gdi(&mut self, now: Instant) {
        let ok = self.cap.set_gdi();
        if ok {
            tracing::info!("GDI capture enabled");
        } else {
            tracing::error!("set_gdi failed; staying on DXGI (30s steady fallback will retry)");
        }
        self.dxgi_probe_deadline = None;
        self.frame_demand_since = None;
        self.dxgi_retry_at = Some(now + DXGI_RETRY_INTERVAL);
        self.would_block_since = None;
    }

    /// GDI → DXGI 试切：cancel_gdi 后由首帧探针裁决（活的 duplication
    /// 首帧必达；仍不可用则探针在窗口期内再次降级）。仅在静默期
    /// （WouldBlock）或输入触发时调用，静态画面下切换无感知。
    fn try_dxgi_again(&mut self, now: Instant) {
        self.cap.cancel_gdi();
        self.dxgi_retry_at = None;
        self.dxgi_probe_deadline = Some(now + DXGI_PROBE_TIMEOUT);
        self.would_block_since = None;
    }

    /// 输入触发的立即回探：输入注入大概率唤醒显示器（物理输入事件重置
    /// 电源空闲计时），DXGI 通常随之恢复——免去最长 DXGI_RETRY_INTERVAL
    /// 的 GDI 驻留。仅 GDI 驻留时有效。
    pub fn retry_dxgi_now(&mut self) {
        if self.cap.is_gdi() {
            tracing::info!("input observed while in GDI, probing DXGI now");
            self.try_dxgi_again(Instant::now());
        }
    }
}

/// WouldBlock 状态下的分层兜底决策（纯函数，单测覆盖分层语义）。
/// DXGI 路由下按优先级判 探针超期 → 需求超时；GDI 驻留下判回探到期。
#[derive(Debug, PartialEq, Eq)]
enum FallbackAction {
    None,
    ProbeFail,
    DemandTimeout,
    RetryDue,
}

fn fallback_action(
    is_gdi: bool,
    probe_deadline: Option<Instant>,
    demand_since: Option<Instant>,
    retry_at: Option<Instant>,
    now: Instant,
) -> FallbackAction {
    if !is_gdi {
        if let Some(dl) = probe_deadline {
            if now >= dl {
                return FallbackAction::ProbeFail;
            }
        }
        if let Some(since) = demand_since {
            if now.duration_since(since) >= FRAME_DEMAND_TIMEOUT {
                return FallbackAction::DemandTimeout;
            }
        }
    } else if let Some(at) = retry_at {
        if now >= at {
            return FallbackAction::RetryDue;
        }
    }
    FallbackAction::None
}

// ---------------- 光标合成（精灵缓存 + 单次采样） ----------------

/// 一次 GetCursorInfo 采样的结果（判变与绘制共用）。
struct CursorSnap {
    hcursor: isize,
    x: i32,
    y: i32,
    visible: bool,
}

/// 预乘 BGRA 光标精灵（构建自 GetIconInfo + GetDIBits，逐帧手工混合）。
struct CursorSprite {
    w: usize,
    h: usize,
    hot_x: i32,
    hot_y: i32,
    /// 行距 = w*4；预乘 Alpha（src over：dst = src + dst*(1-a)）
    data: Vec<u8>,
}

struct CursorPainter {
    /// LRU：表头最近使用。系统光标句柄进程内稳定；应用级自绘光标换图不换
    /// 句柄的极端场景会短暂显示旧精灵（下一次句柄变化自愈）。
    cache: Vec<(isize, Arc<CursorSprite>)>,
    warned: bool,
}

impl CursorPainter {
    fn new() -> Self {
        Self { cache: Vec::new(), warned: false }
    }

    fn sig(s: &CursorSnap) -> (isize, i32, i32, bool) {
        (s.hcursor, s.x, s.y, s.visible)
    }

    /// 单次采样当前光标（失败视为不可见）。
    fn snapshot(&self) -> CursorSnap {
        unsafe {
            let mut ci: winapi::um::winuser::CURSORINFO = std::mem::zeroed();
            ci.cbSize = std::mem::size_of::<winapi::um::winuser::CURSORINFO>() as u32;
            if winapi::um::winuser::GetCursorInfo(&mut ci) == 0 {
                return CursorSnap { hcursor: 0, x: 0, y: 0, visible: false };
            }
            let visible = ci.flags & winapi::um::winuser::CURSOR_SHOWING != 0;
            CursorSnap {
                hcursor: ci.hCursor as isize,
                x: ci.ptScreenPos.x,
                y: ci.ptScreenPos.y,
                visible,
            }
        }
    }

    fn lookup(&mut self, hcursor: isize) -> Option<Arc<CursorSprite>> {
        if let Some(pos) = self.cache.iter().position(|(h, _)| *h == hcursor) {
            let entry = self.cache.remove(pos);
            self.cache.insert(0, entry); // LRU 前移
            Some(self.cache[0].1.clone())
        } else {
            None
        }
    }

    fn insert(&mut self, hcursor: isize, s: Arc<CursorSprite>) {
        self.cache.insert(0, (hcursor, s));
        self.cache.truncate(CURSOR_CACHE_CAP);
    }

    /// 在 BGRA 帧上叠加光标：优先精灵缓存；构建失败回退 DrawIconEx；
    /// 再失败静默跳过（仅告警一次）。
    fn draw(&mut self, bgra: &mut [u8], w: usize, h: usize, snap: &CursorSnap, origin: (i32, i32)) {
        if !snap.visible || snap.hcursor == 0 {
            return;
        }
        let sprite = match self.lookup(snap.hcursor) {
            Some(s) => s,
            None => match build_sprite(snap.hcursor) {
                Some(s) => {
                    let s = Arc::new(s);
                    self.insert(snap.hcursor, s.clone());
                    s
                }
                None => {
                    self.draw_iconex_fallback(bgra, w, h, snap, origin);
                    return;
                }
            },
        };
        let x = snap.x - origin.0 - sprite.hot_x;
        let y = snap.y - origin.1 - sprite.hot_y;
        blend_sprite(bgra, w, h, &sprite, x, y);
    }

    /// 旧路径兜底（GetDIBits 失败时；行为对齐修复前的 DrawIconEx 合成）。
    fn draw_iconex_fallback(&mut self, bgra: &mut [u8], w: usize, h: usize, snap: &CursorSnap, origin: (i32, i32)) {
        unsafe {
            let mut ii: winapi::um::winuser::ICONINFO = std::mem::zeroed();
            if winapi::um::winuser::GetIconInfo(snap.hcursor as winapi::shared::windef::HCURSOR, &mut ii) == 0 {
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

            // 画光标（热点偏移 + 虚拟屏原点；位置 clamp 到帧内）
            let x = (snap.x - origin.0 - ii.xHotspot as i32).clamp(0, w as i32 - 1);
            let y = (snap.y - origin.1 - ii.yHotspot as i32).clamp(0, h as i32 - 1);
            winapi::um::winuser::DrawIconEx(
                hdc_mem, x, y, snap.hcursor as winapi::shared::windef::HCURSOR, 0, 0, 0,
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
        if !self.warned {
            tracing::warn!(msg, "cursor compose failed (disabled logging for repeats)");
            self.warned = true;
        }
    }
}

/// 预乘 BGRA 精灵 over 混合到 BGRA 帧（负坐标/越界逐像素裁剪）。
fn blend_sprite(dst: &mut [u8], dw: usize, dh: usize, sprite: &CursorSprite, x: i32, y: i32) {
    for sy in 0..sprite.h {
        let dy = y + sy as i32;
        if dy < 0 || dy >= dh as i32 {
            continue;
        }
        for sx in 0..sprite.w {
            let dx = x + sx as i32;
            if dx < 0 || dx >= dw as i32 {
                continue;
            }
            let si = (sy * sprite.w + sx) * 4;
            let a = sprite.data[si + 3] as u32;
            if a == 0 {
                continue;
            }
            let di = (dy as usize * dw + dx as usize) * 4;
            if a >= 255 {
                dst[di] = sprite.data[si];
                dst[di + 1] = sprite.data[si + 1];
                dst[di + 2] = sprite.data[si + 2];
                dst[di + 3] = 255;
            } else {
                // 预乘源：dst = src + dst*(1-a)
                for c in 0..3 {
                    let s = sprite.data[si + c] as u32;
                    let d = dst[di + c] as u32;
                    dst[di + c] = (s + (d * (255 - a) + 127) / 255).min(255) as u8;
                }
                dst[di + 3] = 255;
            }
        }
    }
}

// ---------------- 精灵构建（GetIconInfo + GetDIBits） ----------------

/// hCursor → 预乘 BGRA 精灵。彩色位图（32bpp）走直读 + 掩码补 alpha；
/// 单色光标（I-beam/十字等，hbmColor 为空）按 AND/XOR 掩码语义展开。
fn build_sprite(hcursor: isize) -> Option<CursorSprite> {
    unsafe {
        let mut ii: winapi::um::winuser::ICONINFO = std::mem::zeroed();
        if winapi::um::winuser::GetIconInfo(hcursor as winapi::shared::windef::HCURSOR, &mut ii) == 0 {
            return None;
        }
        let hdc = winapi::um::winuser::GetDC(std::ptr::null_mut());
        let sprite = if hdc.is_null() {
            None
        } else if !ii.hbmColor.is_null() {
            build_color_sprite(hdc, ii.hbmColor, ii.hbmMask, ii.xHotspot, ii.yHotspot)
        } else {
            build_mono_sprite(hdc, ii.hbmMask, ii.xHotspot, ii.yHotspot)
        };
        // 兜底：全透明精灵毫无意义（任何上游转换异常最终形态就是不可见）
        // ——视为构建失败，走 DrawIconEx 旧路径
        let sprite = match sprite {
            Some(s) if s.data.chunks(4).any(|c| c[3] > 0) => Some(s),
            _ => None,
        };
        if !hdc.is_null() {
            winapi::um::winuser::ReleaseDC(std::ptr::null_mut(), hdc);
        }
        if !ii.hbmMask.is_null() {
            winapi::um::wingdi::DeleteObject(ii.hbmMask as *mut winapi::ctypes::c_void);
        }
        if !ii.hbmColor.is_null() {
            winapi::um::wingdi::DeleteObject(ii.hbmColor as *mut winapi::ctypes::c_void);
        }
        sprite
    }
}

/// 带完整调色板空间的 BITMAPINFO：GetDIBits 对 ≤8bpp 格式会回填 bmiColors
/// 调色板（1bpp = 2 表项 8 字节）——传裸 BITMAPINFOHEADER 强转 *mut
/// BITMAPINFO 会被越界写穿栈（2026-09-09 生产事故：光标精灵字段被调色板
/// 数据 0x00FFFFFF00000000 踩坏，流内光标彻底不可见）。256 表项覆盖全部
/// 调色板格式。
#[repr(C)]
struct BitmapInfo256 {
    bmiHeader: winapi::um::wingdi::BITMAPINFOHEADER,
    bmiColors: [winapi::um::wingdi::RGBQUAD; 256],
}

impl BitmapInfo256 {
    fn new() -> Self {
        // 全零初始化（POD 结构）；biSize 必填，其余由 GetDIBits 查询回填
        let mut bi: Self = unsafe { std::mem::zeroed() };
        bi.bmiHeader.biSize =
            std::mem::size_of::<winapi::um::wingdi::BITMAPINFOHEADER>() as u32;
        bi
    }
    fn as_info(&mut self) -> *mut winapi::um::wingdi::BITMAPINFO {
        self as *mut _ as *mut winapi::um::wingdi::BITMAPINFO
    }
}

/// 位图 → 32bpp top-down BGRA 像素（两次 GetDIBits：先查尺寸再取位）。
unsafe fn dibits_32bpp(
    hdc: winapi::shared::windef::HDC,
    hbmp: winapi::shared::windef::HBITMAP,
) -> Option<(usize, usize, Vec<u8>)> {
    let mut bi = BitmapInfo256::new();
    if winapi::um::wingdi::GetDIBits(
        hdc, hbmp, 0, 0, std::ptr::null_mut(),
        bi.as_info(),
        winapi::um::wingdi::DIB_RGB_COLORS,
    ) == 0 {
        return None;
    }
    let w = bi.bmiHeader.biWidth as usize;
    let h = bi.bmiHeader.biHeight.unsigned_abs() as usize;
    if w == 0 || h == 0 || w > SPRITE_MAX_DIM || h > SPRITE_MAX_DIM {
        return None;
    }
    bi.bmiHeader.biHeight = -(h as i32); // top-down
    bi.bmiHeader.biBitCount = 32;
    bi.bmiHeader.biCompression = winapi::um::wingdi::BI_RGB;
    let mut buf = vec![0u8; w * h * 4];
    if winapi::um::wingdi::GetDIBits(
        hdc, hbmp, 0, h as u32, buf.as_mut_ptr() as *mut winapi::ctypes::c_void,
        bi.as_info(),
        winapi::um::wingdi::DIB_RGB_COLORS,
    ) == 0 {
        return None;
    }
    Some((w, h, buf))
}

/// 位图 → 1bpp 位平面（行按 4 字节对齐）。返回 (宽, 高, 位数据)。
unsafe fn dibits_1bpp(
    hdc: winapi::shared::windef::HDC,
    hbmp: winapi::shared::windef::HBITMAP,
) -> Option<(usize, usize, Vec<u8>)> {
    let mut bi = BitmapInfo256::new();
    if winapi::um::wingdi::GetDIBits(
        hdc, hbmp, 0, 0, std::ptr::null_mut(),
        bi.as_info(),
        winapi::um::wingdi::DIB_RGB_COLORS,
    ) == 0 {
        return None;
    }
    let w = bi.bmiHeader.biWidth as usize;
    let h = bi.bmiHeader.biHeight.unsigned_abs() as usize;
    if w == 0 || h == 0 || w > SPRITE_MAX_DIM || h > SPRITE_MAX_DIM * 2 {
        return None;
    }
    let stride = ((w + 31) / 32) * 4;
    bi.bmiHeader.biHeight = -(h as i32); // top-down
    bi.bmiHeader.biBitCount = 1;
    bi.bmiHeader.biCompression = winapi::um::wingdi::BI_RGB;
    let mut buf = vec![0u8; stride * h];
    if winapi::um::wingdi::GetDIBits(
        hdc, hbmp, 0, h as u32, buf.as_mut_ptr() as *mut winapi::ctypes::c_void,
        bi.as_info(),
        winapi::um::wingdi::DIB_RGB_COLORS,
    ) == 0 {
        return None;
    }
    Some((w, h, buf))
}

/// 单色掩码取位：bit=1 表示白（对 AND 掩码即“透明”）。
fn mask_bit(bits: &[u8], stride: usize, x: usize, y: usize) -> u8 {
    (bits[y * stride + x / 8] >> (7 - (x % 8))) & 1
}

/// 彩色光标：32bpp 位图（Windows 存预乘 BGRA）+ AND 掩码补老式光标的
/// 缺失 alpha（alpha==0 且掩码不透明 → 不透明着色）。
unsafe fn build_color_sprite(
    hdc: winapi::shared::windef::HDC,
    hbm_color: winapi::shared::windef::HBITMAP,
    hbm_mask: winapi::shared::windef::HBITMAP,
    hot_x: u32,
    hot_y: u32,
) -> Option<CursorSprite> {
    let (w, h, mut data) = dibits_32bpp(hdc, hbm_color)?;
    let mask = dibits_1bpp(hdc, hbm_mask);
    if let Some((mw, mh, bits)) = &mask {
        let stride = ((*mw + 31) / 32) * 4;
        // 彩色光标的掩码是单倍高 AND 掩码（2026-09-09 本机探针证实：
        // 标准 arrow 的 mask 32x32 == color 32x32；双倍高只属于单色光标）
        let and_h = *mh;
        for y in 0..h.min(and_h) {
            for x in 0..w.min(*mw) {
                let i = (y * w + x) * 4;
                if data[i + 3] == 0 && mask_bit(bits, stride, x, y) == 0 {
                    data[i + 3] = 255;
                }
            }
        }
    }
    Some(CursorSprite { w, h, hot_x: hot_x as i32, hot_y: hot_y as i32, data })
}

/// 单色光标（hbmColor 空）：掩码位图高两倍——上半 AND、下半 XOR。
/// (AND,XOR)：(0,0)=黑 (0,1)=白 (1,0)=透明 (1,1)=反色。
/// 反色像素（I-beam 的字形边缘等）画白在白底不可见、画黑在黑底不可见——
/// TigerVNC/rustdesk 的处理是“画黑 + 外扩 1px 白环描边”（热点 +1），任何
/// 背景都可见；无反色像素时不外扩，1:1 展开。
unsafe fn build_mono_sprite(
    hdc: winapi::shared::windef::HDC,
    hbm_mask: winapi::shared::windef::HBITMAP,
    hot_x: u32,
    hot_y: u32,
) -> Option<CursorSprite> {
    #[derive(Clone, Copy, PartialEq)]
    enum Px {
        Transparent,
        Black,
        White,
        Invert,
    }
    let (mw, mh, bits) = dibits_1bpp(hdc, hbm_mask)?;
    if mh % 2 != 0 || mw == 0 {
        return None;
    }
    let h = mh / 2;
    let stride = ((mw + 31) / 32) * 4;
    let mut px = vec![Px::Transparent; mw * h];
    let mut has_invert = false;
    for y in 0..h {
        for x in 0..mw {
            let and = mask_bit(&bits, stride, x, y);
            let xor = mask_bit(&bits, stride, x, y + h);
            px[y * mw + x] = match (and, xor) {
                (0, 0) => Px::Black,
                (0, 1) => Px::White,
                (1, 0) => Px::Transparent,
                _ => {
                    has_invert = true;
                    Px::Invert
                }
            };
        }
    }

    fn set(data: &mut [u8], w: usize, x: usize, y: usize, black: bool) {
        let i = (y * w + x) * 4;
        if black {
            data[i + 3] = 255; // BGRA 全 0 + 不透明 = 黑
        } else {
            data[i] = 255;
            data[i + 1] = 255;
            data[i + 2] = 255;
            data[i + 3] = 255;
        }
    }

    if !has_invert {
        let mut data = vec![0u8; mw * h * 4];
        for (i, p) in px.iter().enumerate() {
            match p {
                Px::Transparent => {}
                Px::Black => set(&mut data, mw, i % mw, i / mw, true),
                _ => set(&mut data, mw, i % mw, i / mw, false),
            }
        }
        return Some(CursorSprite { w: mw, h, hot_x: hot_x as i32, hot_y: hot_y as i32, data });
    }

    // 反色形态：先给所有不透明像素画 1px 白环，再画本体（反色画黑 =
    // 反色的可见下界近似）
    let (ow, oh) = (mw + 2, h + 2);
    let mut data = vec![0u8; ow * oh * 4];
    for y in 0..h {
        for x in 0..mw {
            if px[y * mw + x] == Px::Transparent {
                continue;
            }
            for dy in -1i32..=1 {
                for dx in -1i32..=1 {
                    if dx == 0 && dy == 0 {
                        continue;
                    }
                    let ox = x as i32 + dx + 1;
                    let oy = y as i32 + dy + 1;
                    if ox < 0 || oy < 0 || ox >= ow as i32 || oy >= oh as i32 {
                        continue;
                    }
                    let i = (oy as usize * ow + ox as usize) * 4;
                    if data[i + 3] == 0 {
                        data[i] = 255;
                        data[i + 1] = 255;
                        data[i + 2] = 255;
                        data[i + 3] = 255;
                    }
                }
            }
        }
    }
    for y in 0..h {
        for x in 0..mw {
            match px[y * mw + x] {
                Px::Transparent => {}
                Px::Black | Px::Invert => set(&mut data, ow, x + 1, y + 1, true),
                Px::White => set(&mut data, ow, x + 1, y + 1, false),
            }
        }
    }
    Some(CursorSprite { w: ow, h: oh, hot_x: hot_x as i32 + 1, hot_y: hot_y as i32 + 1, data })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    fn sprite(w: usize, h: usize, data: Vec<u8>) -> CursorSprite {
        CursorSprite { w, h, hot_x: 0, hot_y: 0, data }
    }

    /// 分层兜底决策的边界语义（2026-09-15 规范 §1.2）：
    /// 探针超期优先于需求超时；两者仅 DXGI 路由生效；GDI 驻留只看回探。
    #[test]
    fn fallback_action_layering() {
        let now = Instant::now();
        let past = |ms: u64| now - Duration::from_millis(ms);
        let future = |ms: u64| now + Duration::from_millis(ms);

        // DXGI 健康在途：探针未超期、无需求 → 无动作
        assert_eq!(
            fallback_action(false, Some(future(1500)), None, None, now),
            FallbackAction::None
        );
        // 探针超期（需求同时已超时也由探针优先裁决）
        assert_eq!(
            fallback_action(false, Some(past(1)), Some(past(6000)), None, now),
            FallbackAction::ProbeFail
        );
        // 需求超时的边界：4999ms 未到、5000ms 到点
        assert_eq!(
            fallback_action(false, None, Some(past(4999)), None, now),
            FallbackAction::None
        );
        assert_eq!(
            fallback_action(false, None, Some(past(5000)), None, now),
            FallbackAction::DemandTimeout
        );
        // GDI 驻留：回探未到期无动作，到期 RetryDue；探针/需求不再触发
        assert_eq!(
            fallback_action(true, None, None, Some(future(1000)), now),
            FallbackAction::None
        );
        assert_eq!(
            fallback_action(true, None, None, Some(past(1)), now),
            FallbackAction::RetryDue
        );
        assert_eq!(
            fallback_action(true, Some(past(2000)), Some(past(6000)), None, now),
            FallbackAction::None
        );
    }

    /// 本机交互会话诊断探针：真实 GetCursorInfo → GetIconInfo → 精灵转换，
    /// 打印位图几何与 alpha 分布。仅在交互桌面可用（CI 无桌面，标 ignore）：
    /// `cargo test -- --ignored --nocapture probe_cursor`
    #[test]
    #[ignore]
    fn probe_cursor_sprite_conversion() {
        let painter = CursorPainter::new();
        let snap = painter.snapshot();
        println!(
            "snap: hcursor=0x{:x} pos=({},{}) visible={}",
            snap.hcursor, snap.x, snap.y, snap.visible
        );
        // GetIconInfo 位图几何（验证掩码高度假设：彩色光标掩码应为单倍高）
        unsafe {
            let mut ii: winapi::um::winuser::ICONINFO = std::mem::zeroed();
            if winapi::um::winuser::GetIconInfo(snap.hcursor as winapi::shared::windef::HCURSOR, &mut ii) != 0 {
                let mut bm: winapi::um::wingdi::BITMAP = std::mem::zeroed();
                if !ii.hbmMask.is_null()
                    && winapi::um::wingdi::GetObjectW(
                        ii.hbmMask as *mut _,
                        std::mem::size_of::<winapi::um::wingdi::BITMAP>() as i32,
                        &mut bm as *mut _ as *mut _,
                    ) != 0
                {
                    println!("mask bitmap: {}x{} planes={} bpp={}", bm.bmWidth, bm.bmHeight, bm.bmPlanes, bm.bmBitsPixel);
                }
                if !ii.hbmColor.is_null()
                    && winapi::um::wingdi::GetObjectW(
                        ii.hbmColor as *mut _,
                        std::mem::size_of::<winapi::um::wingdi::BITMAP>() as i32,
                        &mut bm as *mut _ as *mut _,
                    ) != 0
                {
                    println!("color bitmap: {}x{} planes={} bpp={}", bm.bmWidth, bm.bmHeight, bm.bmPlanes, bm.bmBitsPixel);
                }
                if !ii.hbmMask.is_null() {
                    winapi::um::wingdi::DeleteObject(ii.hbmMask as *mut _);
                }
                if !ii.hbmColor.is_null() {
                    winapi::um::wingdi::DeleteObject(ii.hbmColor as *mut _);
                }
            } else {
                println!("GetIconInfo failed");
            }
        }
        match build_sprite(snap.hcursor) {
            Some(s) => {
                let opaque = s.data.chunks(4).filter(|c| c[3] > 0).count();
                println!(
                    "sprite: {}x{} hot=({},{}) opaque={}/{} data_len={}",
                    s.w,
                    s.h,
                    s.hot_x,
                    s.hot_y,
                    opaque,
                    s.w * s.h,
                    s.data.len()
                );
                // 12×12 网格采样 alpha，直观看形状
                for gy in 0..12 {
                    let mut row = String::new();
                    for gx in 0..12 {
                        let x = (gx * s.w).max(11) / 12;
                        let y = (gy * s.h).max(11) / 12;
                        let a = s.data[(y * s.w + x) * 4 + 3];
                        row.push(match a {
                            0 => '.',
                            1..=63 => '·',
                            64..=191 => '+',
                            _ => '#',
                        });
                    }
                    println!("  |{}|", row);
                }
            }
            None => println!("build_sprite returned None"),
        }
    }

    #[test]
    fn blend_opaque_overwrites() {
        let mut dst = vec![9u8; 2 * 2 * 4];
        let sp = sprite(1, 1, vec![10, 20, 30, 255]);
        blend_sprite(&mut dst, 2, 2, &sp, 1, 1);
        let i = (1 * 2 + 1) * 4;
        assert_eq!(&dst[i..i + 4], &[10, 20, 30, 255]);
        assert_eq!(dst[0], 9); // 其余不动
    }

    #[test]
    fn blend_transparent_skips() {
        let mut dst = vec![9u8; 4];
        let sp = sprite(1, 1, vec![10, 20, 30, 0]);
        blend_sprite(&mut dst, 1, 1, &sp, 0, 0);
        assert_eq!(dst[0], 9);
    }

    #[test]
    fn blend_partial_premultiplied() {
        // 预乘 src(B=128,a=128) over dst(B=255)：128 + 255*127/255 ≈ 255
        let mut dst = vec![255u8, 0, 0, 255];
        let sp = sprite(1, 1, vec![128, 0, 0, 128]);
        blend_sprite(&mut dst, 1, 1, &sp, 0, 0);
        assert_eq!(dst[0], 255);
        // 预乘 src(B=0,a=128) over dst(B=255)：0 + 127 = 127
        let mut dst = vec![255u8, 0, 0, 255];
        let sp = sprite(1, 1, vec![0, 0, 0, 128]);
        blend_sprite(&mut dst, 1, 1, &sp, 0, 0);
        assert_eq!(dst[0], 127);
    }

    #[test]
    fn blend_clips_out_of_bounds() {
        // 精灵中心压在画布角落：负坐标部分被裁掉，不 panic
        let mut dst = vec![0u8; 3 * 3 * 4];
        let sp = sprite(2, 2, [[200u8, 200, 200, 255]; 8].concat());
        blend_sprite(&mut dst, 3, 3, &sp, -1, -1);
        // 只有 (0,0) 落在画布内
        assert_eq!(&dst[0..4], &[200, 200, 200, 255]);
        assert_eq!(dst[4], 0);
        // 完全出界：无操作
        blend_sprite(&mut dst, 3, 3, &sp, 100, 100);
        assert_eq!(dst[4], 0);
    }
}
