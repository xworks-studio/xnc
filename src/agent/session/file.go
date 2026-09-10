// file.go — 会话 kind=file 的文件传输。upload：binary 帧流写入 .xnc-part
// 临时文件，size+sha256 双校验通过后原子 rename 落盘，任何失败路径删半成品
// 并回 FILE_ERROR；download：FILE_BEGIN 开场 → binary 帧全量推送（边送边算
// hash）→ FILE_RESULT 携带实测 sha256。FILE_BEGIN/FILE_RESULT/FILE_ERROR 为
// kind 私有 text 词汇，不入 proto（同 typeExecResult 的归属规则）。
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/coder/websocket"

	"xnc/proto"
)

const (
	typeFileBegin  = "FILE_BEGIN"
	typeFileResult = "FILE_RESULT"
	typeFileError  = "FILE_ERROR"

	fileChunkSize = 64 * 1024

	// fileMaxBytes download 兜底上限 256MB，与 server 侧
	// file_handlers.go 的上传上限同值对称：防篡改路径绕过服务端校验后
	// 经 download 无界拉取超大文件。
	fileMaxBytes = 256 * 1024 * 1024
)

// File 文件传输处理器。Log 为 nil 时用 slog.Default()。
type File struct{ Log *slog.Logger }

func NewFile(log *slog.Logger) *File { return &File{Log: log} }

func (f *File) logger() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

// Handle 按 Direction 分发。参数不可解或缺 path 视为不可达文件：FILE_ERROR
// 后关线。
func (f *File) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p proto.FileParams
	if err := json.Unmarshal(params, &p); err != nil || p.Path == "" {
		f.fileError(ctx, ws, proto.CodeFileNotFound, "")
		return
	}
	switch p.Direction {
	case "upload":
		f.handleUpload(ctx, ws, sessionID, p)
	case "download":
		f.handleDownload(ctx, ws, sessionID, p)
	default:
		f.fileError(ctx, ws, proto.CodeFileNotFound, "")
	}
}

// fileAccessErrCode 把写路径失败（os.MkdirAll/os.Create 的 *PathError、
// os.Rename 的 *LinkError）映射为 FILE_ERROR 码：底层 errno 属权限/占用类
// （ERROR_ACCESS_DENIED、共享冲突、EACCES/EPERM）→ ACCESS_DENIED；其余
// （跨卷 EXDEV 等）→ INTERNAL。不再用 FILE_NOT_FOUND 掩盖「目标存在但写
// 不了」——典型场景：put 覆盖运行中的 exe（Windows 返回访问被拒绝）。
func fileAccessErrCode(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) && errnoIsAccessDenied(errno) {
		return proto.CodeAccessDenied
	}
	return proto.CodeInternal
}

// handleUpload upload 方向：先落 .xnc-part 临时文件并发 FILE_BEGIN，再消费
// binary 帧直至声明 size 收满（超量即拒 FILE_TOO_LARGE，对端提前断开落入
// mismatch 路径）；hash/size 校验失败删半成品回 FILE_ERROR（HASH_MISMATCH），
// 通过则 rename 为目标路径并回 FILE_RESULT ok=true。
func (f *File) handleUpload(ctx context.Context, ws *websocket.Conn, sessionID string, p proto.FileParams) {
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
		f.logger().Warn("file upload mkdir failed", "session", sessionID, "err", err)
		f.fileError(ctx, ws, fileAccessErrCode(err), err.Error())
		return
	}
	tmp := p.Path + ".xnc-part"
	fp, err := os.Create(tmp)
	if err != nil {
		f.logger().Warn("file upload create failed", "session", sessionID, "err", err)
		f.fileError(ctx, ws, fileAccessErrCode(err), err.Error())
		return
	}
	defer func() { // 兜底清理：任何退出路径临时文件不得残留（成功 rename 后已不存在）
		_ = fp.Close()
		if _, err := os.Stat(tmp); err == nil {
			_ = os.Remove(tmp)
		}
	}()

	f.writeText(ctx, ws, typeFileBegin, proto.FileBegin{
		Direction: p.Direction, Path: p.Path, Size: p.Size, Sha256: p.Sha256,
	})

	// 读 binary 直到 size 收满：边写边算 sha256；text 帧忽略（不应出现）。
	h := sha256.New()
	var total int64
	for total < p.Size {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			break // 对端提前断开：数据不足，落入下方 mismatch 路径
		}
		if typ != websocket.MessageBinary {
			continue
		}
		if total+int64(len(data)) > p.Size {
			_ = fp.Close()
			_ = os.Remove(tmp)
			f.fileError(ctx, ws, proto.CodeFileTooLarge, "")
			return
		}
		if _, we := fp.Write(data); we != nil {
			f.logger().Warn("file upload write failed", "session", sessionID, "err", we)
			_ = fp.Close()
			_ = os.Remove(tmp)
			f.fileError(ctx, ws, proto.CodeInternal, we.Error())
			return
		}
		_, _ = h.Write(data)
		total += int64(len(data))
	}
	_ = fp.Close() // windows：rename/remove 前必须先关句柄（共享模式不含 DELETE）

	actual := hex.EncodeToString(h.Sum(nil))
	if total != p.Size || actual != p.Sha256 {
		_ = os.Remove(tmp) // 删半成品——须先于 FILE_ERROR 帧（关闭握手会阻塞返回）
		f.fileError(ctx, ws, proto.CodeHashMismatch, "")
		return
	}
	if err := os.Rename(tmp, p.Path); err != nil {
		// 目标被锁/占用（如运行中的 exe）→ ACCESS_DENIED，其余 → INTERNAL；
		// 底层错误文本随 message 透传，不再误导为 FILE_NOT_FOUND。
		f.logger().Warn("file upload rename failed", "session", sessionID, "err", err)
		f.fileError(ctx, ws, fileAccessErrCode(err), err.Error())
		return
	}
	f.writeText(ctx, ws, typeFileResult, proto.FileResult{Bytes: total, Sha256: actual, Ok: true})
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// handleDownload download 方向：FILE_BEGIN（实测 size）→ binary 帧全量推送
// （边送边算 hash）→ FILE_RESULT 携带实测 sha256；文件不可开/不可读回
// FILE_ERROR，超 256MB 上限回 FILE_TOO_LARGE，或静默终止（写失败=对端已断）。
func (f *File) handleDownload(ctx context.Context, ws *websocket.Conn, sessionID string, p proto.FileParams) {
	fp, err := os.Open(p.Path)
	if err != nil {
		f.fileError(ctx, ws, proto.CodeFileNotFound, "")
		return
	}
	defer func() { _ = fp.Close() }()
	st, err := fp.Stat()
	if err != nil {
		f.fileError(ctx, ws, proto.CodeFileNotFound, "")
		return
	}
	if st.Size() > fileMaxBytes {
		fp.Close()
		f.fileError(ctx, ws, proto.CodeFileTooLarge, "")
		return
	}

	f.writeText(ctx, ws, typeFileBegin, proto.FileBegin{
		Direction: p.Direction, Path: p.Path, Size: st.Size(),
	})

	h := sha256.New()
	buf := make([]byte, fileChunkSize)
	var total int64
	for {
		n, err := fp.Read(buf)
		if n > 0 {
			wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
			we := ws.Write(wctx, websocket.MessageBinary, buf[:n])
			cancel()
			if we != nil {
				return // 对端已断：会话结束
			}
			_, _ = h.Write(buf[:n])
			total += int64(n)
		}
		if err != nil {
			break // io.EOF 正常收尾；其余读错误按截断流收尾（终态 hash 如实）
		}
	}
	f.writeText(ctx, ws, typeFileResult, proto.FileResult{
		Bytes: total, Sha256: hex.EncodeToString(h.Sum(nil)), Ok: true,
	})
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// writeText 发送 kind 私有 text 帧；marshal/写失败静默（会话已死时无意义）。
func (f *File) writeText(ctx context.Context, ws *websocket.Conn, typ string, payload any) {
	m, _ := proto.NewMsg(typ, payload)
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
}

// fileError 错误终态：FILE_ERROR text 帧（code + 可读 message，message 可为
// 空）后以错误码关线。
func (f *File) fileError(ctx context.Context, ws *websocket.Conn, code, msg string) {
	f.writeText(ctx, ws, typeFileError, proto.FileError{Code: code, Message: msg})
	_ = ws.Close(websocket.StatusInternalError, "file error")
}
