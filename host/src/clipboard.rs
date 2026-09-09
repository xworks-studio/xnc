//! 剪贴板同步（纯文本，CF_UNICODETEXT 双向）。
//!
//! 协议（经 relay 透明转发，零 relay 改动——viewer→host 走 input 信封吃
//! 租约+cap.input 门控，host→viewer 走 legquic default 广播路径）：
//! - 粘贴：viewer 分块发 `{type:"input",event:"clipboard",kind:"set-text",
//!   seq,index,total,text}`（块 ≤16KiB UTF-8——WS 兜底腿无 SetReadLimit，
//!   coder/websocket 默认 32KiB 读上限，留 JSON 开销余量）；全部到齐后
//!   worker 写入 Windows 剪贴板并回 `{type:"clipboard",event:"set-ack",
//!   seq,ok,bytes}`；web 收 ack 才补发合成 Ctrl+V（时序闭环，避免粘贴出
//!   旧内容）。
//! - 复制：主采集循环限频轮询 GetClipboardSequenceNumber（无需开剪贴板，
//!   廉价），变化即读文本、分块推送 `{type:"clipboard",event:"text-chunk"
//!   /"text-end"}`；自己刚写入的序号不回播（回环抑制）。
//!
//! 线程纪律：OpenClipboard 会被剪贴板管理器等短暂占用——写入走专用
//! worker 线程从容重试（不阻塞 tokio 控制任务）；读取在采集线程每 tick
//! 只尝试一次，Busy 下轮再来（不阻塞 frame pacing）。
//!
//! 安全纪律：日志与 ack 只记长度/序号，**永不记录剪贴板内容**（对齐
//! 2026-08-22 rustdesk-inspired 设计"审计不记录剪贴板内容"）。

use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::mpsc;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use serde_json::{json, Value};

use crate::shared::{ControlMsg, Shared};

/// 单块上限（UTF-8 字节）：必须显著低于 WS 兜底腿 32KiB 读上限。
pub const CHUNK_BYTES: usize = 16 * 1024;
/// 同步文本总量上限：超限截断（码点边界）并在 text-end 标记 truncated。
pub const MAX_TEXT_BYTES: usize = 256 * 1024;
/// 分块数上限（DoS 防护：异常 total 直接丢弃）。
const MAX_PARTS: usize = 64;
/// 轮询最小间隔（毫秒）。
const POLL_INTERVAL_MS: u64 = 200;

static WORKER_TX: OnceLock<mpsc::Sender<ClipCmd>> = OnceLock::new();
/// 自己最近一次写入产生的剪贴板序号（回环抑制）。
static ANNOUNCED_SEQ: AtomicU32 = AtomicU32::new(0);
/// 轮询状态（仅采集主线程访问，Mutex 只为 const 初始化）。
static POLL: Mutex<PollState> = Mutex::new(PollState { next_at_ms: 0, last_seq: 0 });

struct PollState {
    next_at_ms: u64,
    last_seq: u32,
}

enum ClipCmd {
    Chunk {
        seq: u64,
        index: usize,
        total: usize,
        text: String,
    },
}

/// 进程级初始化（main 调用一次；pipeline 重启不重复建）。
pub fn init(shared: Arc<Shared>) {
    let (tx, rx) = mpsc::channel();
    if WORKER_TX.set(tx).is_err() {
        return;
    }
    let _ = std::thread::Builder::new()
        .name("xnc-clipboard".into())
        .spawn(move || worker_loop(rx, shared));
}

/// input 事件入口：viewer 粘贴分块（input::handle 转来，已在租约门控内）。
pub fn on_viewer_chunk(v: &Value) {
    if v["kind"].as_str() != Some("set-text") {
        return;
    }
    let (Some(seq), Some(index), Some(total), Some(text)) = (
        v["seq"].as_u64(),
        v["index"].as_u64(),
        v["total"].as_u64(),
        v["text"].as_str(),
    ) else {
        return;
    };
    let Some(tx) = WORKER_TX.get() else { return };
    let _ = tx.send(ClipCmd::Chunk {
        seq,
        index: index as usize,
        total: total as usize,
        text: text.to_string(),
    });
}

/// 采集循环每帧调用（内部限频）。读取远端剪贴板并推送给 viewer。
pub fn poll(shared: &Shared) {
    let Ok(mut st) = POLL.lock() else { return };
    let now_ms = now_unix_ms();
    if now_ms < st.next_at_ms {
        return;
    }
    st.next_at_ms = now_ms + POLL_INTERVAL_MS;

    let seq = unsafe { winapi::um::winuser::GetClipboardSequenceNumber() };
    if seq == 0 || seq == st.last_seq {
        return;
    }
    if seq == ANNOUNCED_SEQ.load(Ordering::Relaxed) {
        st.last_seq = seq; // 自己刚写入：回环抑制，不回播
        return;
    }
    match read_clipboard_text() {
        // Busy（被他进程占用）：不动 last_seq，下轮重试
        ReadOutcome::Busy => {}
        // 非文本变化（图片/文件复制）：标记已见，不推送
        ReadOutcome::NoText => st.last_seq = seq,
        ReadOutcome::Text(text) => {
            st.last_seq = seq;
            if text.is_empty() {
                return; // 清空剪贴板不推送
            }
            let (t, truncated) = truncate_utf8(&text, MAX_TEXT_BYTES);
            let chunks = chunk_utf8(t, CHUNK_BYTES);
            let total = chunks.len();
            for (i, c) in chunks.iter().enumerate() {
                shared.ctrl_send(ControlMsg(json!({
                    "type": "clipboard", "event": "text-chunk",
                    "seq": seq, "index": i, "total": total, "text": c,
                })));
            }
            shared.ctrl_send(ControlMsg(json!({
                "type": "clipboard", "event": "text-end",
                "seq": seq, "truncated": truncated,
            })));
            tracing::info!(bytes = t.len(), seq, "clipboard text pushed to viewers");
        }
    }
}

fn worker_loop(rx: mpsc::Receiver<ClipCmd>, shared: Arc<Shared>) {
    let mut asm = Assembler::default();
    while let Ok(cmd) = rx.recv() {
        match cmd {
            ClipCmd::Chunk { seq, index, total, text } => {
                let Some(full) = asm.push(seq, index, total, &text) else { continue };
                let (t, _truncated) = truncate_utf8(&full, MAX_TEXT_BYTES);
                let ok = set_clipboard_text(t);
                if ok {
                    ANNOUNCED_SEQ.store(
                        unsafe { winapi::um::winuser::GetClipboardSequenceNumber() },
                        Ordering::Relaxed,
                    );
                }
                // 审计纪律：只回长度与结果，不回内容
                shared.ctrl_send(ControlMsg(json!({
                    "type": "clipboard", "event": "set-ack",
                    "seq": seq, "ok": ok, "bytes": t.len(),
                })));
                if !ok {
                    tracing::warn!(seq, bytes = t.len(), "clipboard set-text failed");
                }
            }
        }
    }
}

// ---------------- 分块组装（纯逻辑，单测覆盖） ----------------

#[derive(Default)]
struct Assembler {
    cur: Option<Assembly>,
}

struct Assembly {
    seq: u64,
    total: usize,
    parts: Vec<Option<String>>,
    received: usize,
    bytes: usize,
}

impl Assembler {
    /// 压入一块；全部到齐返回完整文本并清空状态。
    /// 乱序到达可用；新 seq 到达丢弃旧 seq 半截缓冲；超限丢弃整个 seq。
    fn push(&mut self, seq: u64, index: usize, total: usize, text: &str) -> Option<String> {
        if total == 0 || total > MAX_PARTS || index >= total {
            return None;
        }
        let stale = match &self.cur {
            Some(a) => a.seq != seq || a.total != total,
            None => true,
        };
        if stale {
            if self.cur.is_some() {
                tracing::debug!(seq, "clipboard assembly reset (new seq/total)");
            }
            self.cur = Some(Assembly {
                seq,
                total,
                parts: (0..total).map(|_| None).collect(),
                received: 0,
                bytes: 0,
            });
        }
        let a = self.cur.as_mut().unwrap();
        if a.bytes + text.len() > MAX_TEXT_BYTES {
            self.cur = None; // 超限：丢弃整个 seq（web 侧本已截断，此处为防线）
            tracing::warn!(seq, "clipboard assembly over size cap, dropped");
            return None;
        }
        if a.parts[index].is_none() {
            a.parts[index] = Some(text.to_string());
            a.received += 1;
            a.bytes += text.len();
        }
        if a.received == a.total {
            let done: String = a.parts.iter().map(|p| p.as_deref().unwrap_or("")).collect();
            self.cur = None;
            Some(done)
        } else {
            None
        }
    }
}

/// UTF-8 安全切块：块边界回退到码点边界，绝不切断多字节字符。
pub fn chunk_utf8(s: &str, max_bytes: usize) -> Vec<&str> {
    let mut out = Vec::new();
    let mut start = 0usize;
    while s.len() - start > max_bytes {
        let mut end = start + max_bytes;
        while end > start && !s.is_char_boundary(end) {
            end -= 1;
        }
        if end == start {
            end = start + 4.min(s.len() - start); // max_bytes 过小的退化保护
        }
        out.push(&s[start..end]);
        start = end;
    }
    if start < s.len() {
        out.push(&s[start..]);
    }
    out
}

/// 码点边界截断；返回（截断后文本, 是否截断）。
pub fn truncate_utf8(s: &str, max_bytes: usize) -> (&str, bool) {
    if s.len() <= max_bytes {
        return (s, false);
    }
    let mut end = max_bytes;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    (&s[..end], true)
}

// ---------------- Windows 剪贴板访问 ----------------

enum ReadOutcome {
    Text(String),
    /// 剪贴板里没有 CF_UNICODETEXT（图片/文件等）
    NoText,
    /// OpenClipboard 失败（被他进程占用）
    Busy,
}

/// 读取剪贴板文本。只尝试一次 OpenClipboard（采集线程调用，绝不重试阻塞）。
fn read_clipboard_text() -> ReadOutcome {
    unsafe {
        if winapi::um::winuser::OpenClipboard(std::ptr::null_mut()) == 0 {
            return ReadOutcome::Busy;
        }
        let out = {
            let h = winapi::um::winuser::GetClipboardData(winapi::um::winuser::CF_UNICODETEXT);
            if h.is_null() {
                ReadOutcome::NoText
            } else {
                let size = winapi::um::winbase::GlobalSize(h as winapi::shared::minwindef::HGLOBAL);
                let p = winapi::um::winbase::GlobalLock(h as winapi::shared::minwindef::HGLOBAL)
                    as *const u16;
                if p.is_null() {
                    ReadOutcome::NoText
                } else {
                    // GlobalSize 含尾部 NUL；按实际内容截到首个 NUL
                    let cap = size / 2;
                    let mut len = 0usize;
                    while len + 1 < cap && *p.add(len) != 0 {
                        len += 1;
                    }
                    let slice = std::slice::from_raw_parts(p, len);
                    let s = String::from_utf16_lossy(slice);
                    winapi::um::winbase::GlobalUnlock(h as winapi::shared::minwindef::HGLOBAL);
                    ReadOutcome::Text(s)
                }
            }
        };
        winapi::um::winuser::CloseClipboard();
        out
    }
}

/// 写入剪贴板文本（worker 线程调用，从容重试打开）。
fn set_clipboard_text(text: &str) -> bool {
    let mut wchars: Vec<u16> = text.encode_utf16().collect();
    wchars.push(0); // NUL 终止
    let byte_len = wchars.len() * 2;
    unsafe {
        let mut opened = false;
        for _ in 0..20 {
            if winapi::um::winuser::OpenClipboard(std::ptr::null_mut()) != 0 {
                opened = true;
                break;
            }
            std::thread::sleep(Duration::from_millis(25));
        }
        if !opened {
            return false;
        }
        let ok = (|| {
            if winapi::um::winuser::EmptyClipboard() == 0 {
                return false;
            }
            let h = winapi::um::winbase::GlobalAlloc(
                winapi::um::winbase::GMEM_MOVEABLE,
                byte_len,
            );
            if h.is_null() {
                return false;
            }
            let dst = winapi::um::winbase::GlobalLock(h) as *mut u8;
            if dst.is_null() {
                winapi::um::winbase::GlobalFree(h);
                return false;
            }
            std::ptr::copy_nonoverlapping(wchars.as_ptr() as *const u8, dst, byte_len);
            winapi::um::winbase::GlobalUnlock(h);
            if winapi::um::winuser::SetClipboardData(
                winapi::um::winuser::CF_UNICODETEXT,
                h as winapi::shared::ntdef::HANDLE,
            )
            .is_null()
            {
                winapi::um::winbase::GlobalFree(h); // 失败：所有权仍在调用方
                false
            } else {
                true // 成功：内存移交系统，勿再释放
            }
        })();
        winapi::um::winuser::CloseClipboard();
        ok
    }
}

fn now_unix_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn chunk_roundtrip_ascii() {
        let s = "a".repeat(CHUNK_BYTES * 3 + 10);
        let chunks = chunk_utf8(&s, CHUNK_BYTES);
        assert_eq!(chunks.len(), 4);
        assert!(chunks.iter().all(|c| c.len() <= CHUNK_BYTES));
        assert_eq!(chunks.concat(), s);
    }

    #[test]
    fn chunk_never_splits_codepoint() {
        // 全 CJK（3 字节/字）：块边界必然落在字符间
        let s = "中".repeat(CHUNK_BYTES); // 3*16KiB 字节
        let chunks = chunk_utf8(&s, CHUNK_BYTES);
        assert!(chunks.iter().all(|c| c.len() <= CHUNK_BYTES));
        assert_eq!(chunks.concat(), s);
        for c in &chunks {
            assert!(std::str::from_utf8(c.as_bytes()).is_ok());
        }
    }

    #[test]
    fn chunk_emoji_surrogate_pairs() {
        let s = "🎉".repeat(10_000); // 4 字节/emoji
        let chunks = chunk_utf8(&s, CHUNK_BYTES);
        assert_eq!(chunks.concat(), s);
    }

    #[test]
    fn truncate_at_boundary() {
        assert_eq!(truncate_utf8("hello", 10), ("hello", false));
        let (t, trunc) = truncate_utf8("中中中", 7); // 7 字节 = 2 个“中”+1 字节
        assert_eq!(t, "中中");
        assert!(trunc);
    }

    #[test]
    fn assembler_in_order_and_out_of_order() {
        let mut a = Assembler::default();
        assert!(a.push(1, 0, 2, "hello ").is_none());
        assert_eq!(a.push(1, 1, 2, "world").unwrap(), "hello world");

        // 乱序
        let mut a = Assembler::default();
        assert!(a.push(7, 1, 2, "B").is_none());
        assert_eq!(a.push(7, 0, 2, "A").unwrap(), "AB");
    }

    #[test]
    fn assembler_new_seq_resets_stale() {
        let mut a = Assembler::default();
        assert!(a.push(1, 0, 3, "x").is_none());
        // 新 seq 到达：旧半截丢弃
        assert!(a.push(2, 0, 2, "p").is_none());
        assert_eq!(a.push(2, 1, 2, "q").unwrap(), "pq");
    }

    #[test]
    fn assembler_rejects_bad_and_oversize() {
        let mut a = Assembler::default();
        assert!(a.push(1, 0, 0, "x").is_none()); // total=0
        assert!(a.push(1, 5, 2, "x").is_none()); // index 越界
        assert!(a.push(1, 0, MAX_PARTS + 1, "x").is_none());
        // 超限：整个 seq 丢弃，后续同 seq 块重新组（旧状态已清）
        let mut a = Assembler::default();
        let big = "x".repeat(MAX_TEXT_BYTES);
        assert!(a.push(1, 0, 2, &big).is_none());
        assert!(a.push(1, 1, 2, "more").is_none());
    }

    #[test]
    fn assembler_duplicate_chunk_is_idempotent() {
        let mut a = Assembler::default();
        assert!(a.push(1, 0, 2, "A").is_none());
        assert!(a.push(1, 0, 2, "A").is_none()); // 重复块不重复计数
        assert_eq!(a.push(1, 1, 2, "B").unwrap(), "AB");
    }
}
