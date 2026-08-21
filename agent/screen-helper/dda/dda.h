// dda.h — DXGI Desktop Duplication 采集层 C ABI。
//
// 设计约束（与 Go helper 的契约）：
//   - COM 指针绝不跨越边界；只有纯数据（BGRA 字节 + 元数据结构体）过桥。
//   - 错误模型：返回码 + dda_last_error() 字符串（带步骤前缀——排障命脉）。
//   - 缓冲由调用方分配（Go 按 Dims 分配 w*h*4），DLL 只写。
//   - 单实例单线程使用（helper 采集循环独占调用）。
//
// 实现依据：MS DXGIDesktopDuplication sample + ffmpeg vf_ddagrab 对象链
// （NULL 适配器硬件设备 + GetAdapter，经 ddatest.c 在 TB16G7/XIAOXIN 实机
// 验证）。曾用 Go syscall 手工 vtable 调用在本接口上系统性失败
// （DuplicateOutput 0x887A0001，根因未明），故整个 COM 层以 C 实现。
#ifndef XNC_DDA_H
#define XNC_DDA_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define DDA_ABI_VERSION 2

// DLL 构建（/DXNC_DDA_BUILD）时导出符号；selftest 静态链接时无修饰。
#ifdef XNC_DDA_BUILD
#define DDA_API __declspec(dllexport)
#else
#define DDA_API
#endif

// dda_acquire 返回的帧类型。
enum {
    DDA_FRAME_CONTENT     = 0,  // bgra 已写入新桌面内容
    DDA_FRAME_CURSOR_ONLY = 1,  // 仅光标变化：bgra 未写；取 dda_cursor_* 跟随
    DDA_FRAME_TIMEOUT     = 2,  // 桌面静止（timeout_ms 内无更新）
    DDA_FRAME_ACCESS_LOST = 3,  // 模式切换/独占全屏等：已自动重建，本帧无数据
    DDA_ERR               = -1  // 致命错误：读 dda_last_error()
};

// 指针形状类型（对应 DXGI_OUTDUPL_POINTER_SHAPE_TYPE）。
enum {
    DDA_POINTER_MONOCHROME = 1,
    DDA_POINTER_COLOR      = 2,
    DDA_POINTER_MASKED     = 4,
};

// dda_cursor — 光标状态。位置为热点校正后的桌面坐标（输出左上为原点）。
// 形状字节经 dda_cursor_shape() 获取，行距 pitch 字节，格式见 type。
typedef struct dda_cursor {
    int32_t type;
    int32_t w, h;
    int32_t pitch;
    int32_t len;
    int32_t x, y;
    uint8_t visible;
} dda_cursor;

// dda_frame — 单次 acquire 的结果元数据。布局固定（Go 侧镜像）。
typedef struct dda_frame {
    int32_t kind;
    int32_t reserved;
    int64_t present_time; // LastPresentTime（QPC 单位；0 = 光标/元数据帧）
    uint32_t accumulated;
    dda_cursor cursor;
} dda_frame;

// dda_abi_version 返回 ABI 版本；helper 加载时校验，防部署偏斜。
DDA_API uint32_t dda_abi_version(void);

// dda_last_error 返回最近一次错误的描述（步骤前缀 + HRESULT），静态缓冲。
DDA_API const char *dda_last_error(void);

// dda_create 建立 duplication。成功返回实例并写 w/h（原生分辨率）；
// 失败返回 NULL。枚举所有 attached 输出，取第一个可复制者。
DDA_API void *dda_create(int32_t *w, int32_t *h);

// dda_acquire 拉取一帧。bgra 容量须 ≥ w*h*4。返回 DDA_FRAME_* 之一；
// ACCESS_LOST 已在内部重建（含 staging），调用方直接重试即可。
DDA_API int32_t dda_acquire(void *dda, uint8_t *bgra, int32_t cap_bytes, int32_t timeout_ms, dda_frame *out);

// dda_cursor_shape 返回当前指针形状缓冲（内部所有，随 acquire 更新）。
DDA_API const uint8_t *dda_cursor_shape(const void *dda);

// dda_format 返回 duplication 的 ModeDesc.Format（DXGI_FORMAT 枚举值；87=BGRA8）。
DDA_API int32_t dda_format(const void *dda);
DDA_API void dda_dims(const void *dda, int32_t *w, int32_t *h);

// dda_destroy 释放全部资源。
DDA_API void dda_destroy(void *dda);

#ifdef __cplusplus
}
#endif

#endif // XNC_DDA_H
