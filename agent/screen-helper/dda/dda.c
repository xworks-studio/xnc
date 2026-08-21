// dda.c — DXGI Desktop Duplication 采集实现（C ABI DLL / selftest 共用）。
//
// 关键实现事实（实机验证，勿凭记忆改动）：
//   - 设备链：D3D11CreateDevice(NULL, HARDWARE, VIDEO_SUPPORT|BGRA) →
//     QI(IDXGIDevice) → GetAdapter → EnumOutputs → QI(IDXGIOutput1) →
//     DuplicateOutput。与 ffmpeg vf_ddagrab 一致，ddatest.c 三机验证通过。
//   - 不需要 CoInitializeEx（ddatest.c 实证：无 COM 单元初始化也能建立
//     duplication）。
//   - 尺寸以 duplication 的 GetDesc 为准（原生分辨率，DPI 虚拟化无关）。
//   - ReleaseFrame 纪律：CopyResource + GetFramePointerShape 完成后立即
//     ReleaseFrame，绝不跨迭代持有 duplication 纹理。
//   - 光标形状必须在帧持有期内经 GetFramePointerShape 读取。
//   - ACCESS_LOST：Release+DuplicateOutput 重建；模式可能变了 → 重读
//     GetDesc 并按需重建 staging。
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <d3d11.h>
#include <dxgi1_2.h>
#include <stdio.h>
#include <string.h>
#include "dda.h"

typedef struct dda_ctx {
    ID3D11Device           *dev;
    ID3D11DeviceContext    *ctx;
    IDXGIOutput1           *out1;
    IDXGIOutputDuplication *dupl;
    ID3D11Texture2D        *staging;
    int32_t w, h;

    // 安全桌面 GDI 模式状态（RustDesk 机制，SYSTEM 令牌专属）。
    int secure_mode;
    HDESK hDefault, hSecure;
    HDC hdcScreen, hdcMem;
    HBITMAP hbmp;
    void *dib_bits;
    int32_t gw, gh;

    uint8_t *shape;
    DWORD    shape_cap;
    DXGI_OUTDUPL_POINTER_SHAPE_INFO shape_info;
    dda_cursor cursor;
    int      cursor_valid;

    char err[512];
} dda_ctx;

// 单实例全局（helper 单线程使用；selftest 同样）。
static dda_ctx *dda_ctx_global = NULL;
static char g_create_err[512]; // dda_create 失败时暂存（实例尚不存在）

static void set_err(dda_ctx *c, const char *step, HRESULT hr) {
    if (c == NULL) return;
    _snprintf_s(c->err, sizeof(c->err), _TRUNCATE,
                "%s: hr=0x%08lX", step, (unsigned long)hr);
}
static void set_err_msg(dda_ctx *c, const char *msg) {
    if (c == NULL) return;
    _snprintf_s(c->err, sizeof(c->err), _TRUNCATE, "%s", msg);
}

uint32_t dda_abi_version(void) { return DDA_ABI_VERSION; }

const char *dda_last_error(void) {
    if (dda_ctx_global && dda_ctx_global->err[0]) return dda_ctx_global->err;
    return g_create_err[0] ? g_create_err : "no error";
}

// dupl_desc 读取当前 duplication 尺寸与旋转。
static HRESULT dupl_dims(IDXGIOutputDuplication *dupl, int32_t *w, int32_t *h, DXGI_MODE_ROTATION *rot) {
    DXGI_OUTDUPL_DESC d;
    dupl->lpVtbl->GetDesc(dupl, &d);
    HRESULT hr = S_OK;
    if (SUCCEEDED(hr)) {
        *w = (int32_t)d.ModeDesc.Width;
        *h = (int32_t)d.ModeDesc.Height;
        if (rot) *rot = d.Rotation;
    }
    return hr;
}

// make_staging 按当前尺寸建 CPU 可读 BGRA 纹理。
static HRESULT make_staging(dda_ctx *c) {
    if (c->staging) {
        c->staging->lpVtbl->Release(c->staging);
        c->staging = NULL;
    }
    D3D11_TEXTURE2D_DESC td;
    ZeroMemory(&td, sizeof(td));
    td.Width = (UINT)c->w;
    td.Height = (UINT)c->h;
    td.MipLevels = 1;
    td.ArraySize = 1;
    td.SampleDesc.Count = 1;
    td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
    td.Usage = D3D11_USAGE_STAGING;
    td.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
    return c->dev->lpVtbl->CreateTexture2D(c->dev, &td, NULL, &c->staging);
}

// duplicate 在已持有的 out1/dev 上（重）建 duplication 并同步 staging。
static HRESULT do_duplicate(dda_ctx *c) {
    if (c->dupl) {
        c->dupl->lpVtbl->Release(c->dupl);
        c->dupl = NULL;
    }
    HRESULT hr = c->out1->lpVtbl->DuplicateOutput(c->out1, (IUnknown *)c->dev, &c->dupl);
    if (FAILED(hr)) return hr;
    int32_t w, h;
    DXGI_MODE_ROTATION rot;
    hr = dupl_dims(c->dupl, &w, &h, &rot);
    if (FAILED(hr)) return hr;
    if (rot != DXGI_MODE_ROTATION_IDENTITY) {
        // v1 不支持旋转输出（竖屏等罕见路径），显式报错走回退。
        set_err_msg(c, "rotated output not supported yet");
        return E_NOTIMPL;
    }
    if (w != c->w || h != c->h) {
        c->w = w;
        c->h = h;
        hr = make_staging(c);
        if (FAILED(hr)) return hr;
    }
    return S_OK;
}

void *dda_create(int32_t *w, int32_t *h) {
    g_create_err[0] = '\0';
    dda_ctx *c = (dda_ctx *)calloc(1, sizeof(dda_ctx));
    if (!c) return NULL;
    dda_ctx_global = c;

    HRESULT hr;
    D3D_FEATURE_LEVEL fl = 0;

    // 1. NULL 适配器硬件设备（ffmpeg 同款链）。
    hr = D3D11CreateDevice(NULL, D3D_DRIVER_TYPE_HARDWARE, NULL,
                           D3D11_CREATE_DEVICE_VIDEO_SUPPORT | D3D11_CREATE_DEVICE_BGRA_SUPPORT,
                           NULL, 0, D3D11_SDK_VERSION, &c->dev, &fl, &c->ctx);
    if (FAILED(hr)) { set_err(c, "D3D11CreateDevice", hr); goto fail; }

    // 2. 设备 → 适配器 → 枚举 attached 输出。
    IDXGIDevice *dxgiDev = NULL;
    hr = c->dev->lpVtbl->QueryInterface(c->dev, &IID_IDXGIDevice, (void **)&dxgiDev);
    if (FAILED(hr)) { set_err(c, "QI IDXGIDevice", hr); goto fail; }
    IDXGIAdapter *adapter = NULL;
    hr = dxgiDev->lpVtbl->GetAdapter(dxgiDev, &adapter);
    dxgiDev->lpVtbl->Release(dxgiDev);
    if (FAILED(hr)) { set_err(c, "GetAdapter", hr); goto fail; }

    for (UINT i = 0; ; i++) {
        IDXGIOutput *output = NULL;
        hr = adapter->lpVtbl->EnumOutputs(adapter, i, &output);
        if (hr == DXGI_ERROR_NOT_FOUND) {
            set_err_msg(c, "no desktop-attached output");
            break;
        }
        if (FAILED(hr)) { set_err(c, "EnumOutputs", hr); break; }
        DXGI_OUTPUT_DESC od;
        hr = output->lpVtbl->GetDesc(output, &od);
        if (FAILED(hr) || !od.AttachedToDesktop) {
            output->lpVtbl->Release(output);
            if (FAILED(hr)) { set_err(c, "GetDesc(output)", hr); break; }
            continue;
        }
        IDXGIOutput1 *out1 = NULL;
        hr = output->lpVtbl->QueryInterface(output, &IID_IDXGIOutput1, (void **)&out1);
        output->lpVtbl->Release(output);
        if (FAILED(hr)) { set_err(c, "QI IDXGIOutput1", hr); continue; }
        c->out1 = out1;
        hr = do_duplicate(c);
        if (SUCCEEDED(hr)) {
            adapter->lpVtbl->Release(adapter);
            if (w) *w = c->w;
            if (h) *h = c->h;
            c->err[0] = '\0';
            return c;
        }
        // 该输出复制失败：记录并试下一个输出。
        char last[256];
        strncpy_s(last, sizeof(last), c->err, _TRUNCATE);
        c->out1->lpVtbl->Release(c->out1);
        c->out1 = NULL;
        _snprintf_s(c->err, sizeof(c->err), _TRUNCATE, "output %u: %s", i, last);
    }
    adapter->lpVtbl->Release(adapter);

fail:
    strncpy_s(g_create_err, sizeof(g_create_err), c->err, _TRUNCATE);
    dda_destroy(c);
    dda_ctx_global = NULL;
    return NULL;
}

// fetch_cursor 在帧持有期内读取指针形状（缓冲按需增长）。
static void fetch_cursor(dda_ctx *c, const DXGI_OUTDUPL_FRAME_INFO *info) {
    c->cursor.x = (int32_t)info->PointerPosition.Position.x;
    c->cursor.y = (int32_t)info->PointerPosition.Position.y;
    c->cursor.visible = info->PointerPosition.Visible ? 1 : 0;
    c->cursor_valid = 0;
    if (info->TotalMetadataBufferSize == 0)
        return;
    // 有元数据 → 尝试取形状（无新形状时返回 DXGI_ERROR_MORE_ENTRIES 语义下
    // 沿用上次缓冲——正确姿势：形状未变时指针形状大小请求返回相同缓冲）。
    DXGI_OUTDUPL_POINTER_SHAPE_INFO si;
    DWORD required = 0;
    if (!c->shape) {
        c->shape_cap = 64 * 1024;
        c->shape = (uint8_t *)malloc(c->shape_cap);
        if (!c->shape) return;
    }
    HRESULT hr = c->dupl->lpVtbl->GetFramePointerShape(c->dupl, c->shape_cap, c->shape, &required, &si);
    if (hr == DXGI_ERROR_MORE_DATA && required > 0) {
        uint8_t *nb = (uint8_t *)realloc(c->shape, required);
        if (nb) {
            c->shape = nb;
            c->shape_cap = required;
            hr = c->dupl->lpVtbl->GetFramePointerShape(c->dupl, c->shape_cap, c->shape, &required, &si);
        }
    }
    if (SUCCEEDED(hr)) {
        c->shape_info = si;
        c->cursor.type = (int32_t)si.Type;
        c->cursor.w = (int32_t)si.Width;
        c->cursor.h = (int32_t)si.Height;
        c->cursor.pitch = (int32_t)si.Pitch;
        c->cursor.len = (int32_t)(si.Pitch * si.Height);
        c->cursor_valid = 1;
    }
    // 失败（含形状未变化的语义路径）：沿用上次的 shape/cursor 字段。
}

// ---- 安全桌面（UAC）GDI 路径（RustDesk 同款机制）----
// 依据：DXGI 对安全桌面封闭（重建 E_ACCESSDENIDEN，实测 SYSTEM 亦然）；
// SYSTEM 令牌 + SetThreadDesktop(Winlogon) + BitBlt(CAPTUREBLT) 可捕获
// （gditest.c 于 LABS-XIAOXIN 实证：UAC 弹窗像素完整入帧）。
// helper 需以 SYSTEM-in-session 运行（agent TokenSessionId 启动器）。

// Forward declarations (used early by dda_destroy)
static void secure_gdi_teardown(dda_ctx *c);
static int enter_secure_mode_with(dda_ctx *c, HDESK hIn);
static void enter_secure_mode(dda_ctx *c);
static void leave_secure_mode(dda_ctx *c);
static int32_t secure_gdi_acquire(dda_ctx *c, uint8_t *bgra, int32_t cap, int32_t timeout_ms, dda_frame *out);

static BOOL input_desktop_name(char *name, DWORD cap, HDESK *hOut) {
    HDESK h = OpenInputDesktop(0, FALSE, GENERIC_READ | DESKTOP_READOBJECTS);
    if (!h) return FALSE;
    if (hOut) *hOut = h; else CloseDesktop(h);
    if (name) {
        DWORD need = 0;
        if (!GetUserObjectInformationA(h, UOI_NAME, name, cap, &need)) name[0] = 0;
    }
    return TRUE;
}

int32_t dda_acquire(void *dda, uint8_t *bgra, int32_t cap, int32_t timeout_ms, dda_frame *out) {
    dda_ctx *c = (dda_ctx *)dda;

    // 安全桌面 GDI 模式：ACCESS_LOST 后若输入桌面已切走（UAC），以
    // SYSTEM + SetThreadDesktop + BitBlt 捕获安全桌面帧（RustDesk 机制，
    // gditest.c 实证）；桌面切回 Default 后恢复 DXGI。
    if (c->secure_mode) {
        return secure_gdi_acquire(c, bgra, cap, timeout_ms, out);
    }

    // 无 duplication（变暗期 ACCESS_LOST→重建被拒留下的状态）：先探测
    // 桌面——已切到 winlogon 则进 GDI 模式；仍在 Default 则重建重试
    // （返回 TIMEOUT 而非 DDA_ERR——桌面切换期是暂态，致命错会触发调用
    // 方整体重建，在安全桌面期间重建必败并绕远路）。
    if (!c->dupl) {
        char name[64];
        HDESK hIn = NULL;
        if (input_desktop_name(name, sizeof(name), &hIn)) {
            if (strcmp(name, "Default") != 0) {
                if (enter_secure_mode_with(c, hIn)) {
                    if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_TIMEOUT; }
                    return DDA_FRAME_TIMEOUT;
                }
                // 进入失败（SetThreadDesktop 拒绝）：显式致命错暴露原因，
                // 调用方日志可见——不再静默无限重试（stats 冻结事故）。
                return DDA_ERR;
            }
            if (hIn) CloseDesktop(hIn);
        }
        if (FAILED(do_duplicate(c))) {
            if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_TIMEOUT; }
            return DDA_FRAME_TIMEOUT;
        }
    }

    DXGI_OUTDUPL_FRAME_INFO info;
    ZeroMemory(&info, sizeof(info));
    IDXGIResource *res = NULL;

    HRESULT hr = c->dupl->lpVtbl->AcquireNextFrame(c->dupl, (UINT)timeout_ms, &info, &res);
    if (hr == DXGI_ERROR_WAIT_TIMEOUT) {
        // 空闲期顺带探测桌面切换（ACCESS_LOST 有时滞后/不触发）。
        char name[64];
        HDESK hIn = NULL;
        if (input_desktop_name(name, sizeof(name), &hIn) && strcmp(name, "Default") != 0) {
            CloseDesktop(hIn);
            enter_secure_mode(c);
            if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_TIMEOUT; }
            return DDA_FRAME_TIMEOUT;
        }
        if (hIn) CloseDesktop(hIn);
        if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_TIMEOUT; }
        return DDA_FRAME_TIMEOUT;
    }
    if (hr == DXGI_ERROR_ACCESS_LOST) {
        // 重建失败不升级为致命错误；安全桌面（UAC）活跃期间重建必被拒
        // （E_ACCESSDENIDEN，实测 SYSTEM 亦然）——切 GDI 安全模式（捕获
        // UAC 弹窗）或普通重试。
        char name[64];
        HDESK hIn = NULL;
        if (input_desktop_name(name, sizeof(name), &hIn)) {
            if (strcmp(name, "Default") != 0) {
                if (enter_secure_mode_with(c, hIn)) {
                    if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_TIMEOUT; }
                    return DDA_FRAME_TIMEOUT;
                }
            }
            if (hIn) CloseDesktop(hIn);
        }
        hr = do_duplicate(c);
        if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_ACCESS_LOST; }
        return DDA_FRAME_ACCESS_LOST;
    }
    if (FAILED(hr)) {
        set_err(c, "AcquireNextFrame", hr);
        return DDA_ERR;
    }

    // 光标状态在帧持有期内读取。
    fetch_cursor(c, &info);

    // 关键：AcquireNextFrame 给的是 IDXGIResource*，必须 QI 成
    // ID3D11Texture2D 才能交给 CopyResource——直接指针强转会传入错误
    // vtable 的接口指针，拷贝静默不发生，读到的是未初始化 staging 的
    // 均匀底色（三机全黑帧事故的根因）。
    ID3D11Texture2D *tex = NULL;
    if (res != NULL) {
        hr = res->lpVtbl->QueryInterface(res, &IID_ID3D11Texture2D, (void **)&tex);
        res->lpVtbl->Release(res);
        if (FAILED(hr)) {
            c->dupl->lpVtbl->ReleaseFrame(c->dupl);
            set_err(c, "QI ID3D11Texture2D", hr);
            return DDA_ERR;
        }
    }

    int32_t kind;
    if (tex == NULL) {
        // 光标/元数据-only 帧：无新纹理。
        kind = DDA_FRAME_CURSOR_ONLY;
    } else {
        kind = DDA_FRAME_CONTENT;
        c->ctx->lpVtbl->CopyResource(c->ctx, (ID3D11Resource *)c->staging, (ID3D11Resource *)tex);
        hr = S_OK;
        if (SUCCEEDED(hr)) {
            D3D11_MAPPED_SUBRESOURCE mapped;
            ZeroMemory(&mapped, sizeof(mapped));
            hr = c->ctx->lpVtbl->Map(c->ctx, (ID3D11Resource *)c->staging, 0, D3D11_MAP_READ, 0, &mapped);
            if (SUCCEEDED(hr)) {
                const uint8_t *src = (const uint8_t *)mapped.pData;
                const int32_t stride = c->w * 4;
                for (int32_t row = 0; row < c->h; row++) {
                    memcpy(bgra + (size_t)row * stride,
                           src + (size_t)row * mapped.RowPitch,
                           (size_t)stride);
                }
                c->ctx->lpVtbl->Unmap(c->ctx, (ID3D11Resource *)c->staging, 0);
            }
        }
        tex->lpVtbl->Release(tex);
    }
    // ReleaseFrame 纪律：所有帧路径（内容/光标-only）读完后立即归还。
    c->dupl->lpVtbl->ReleaseFrame(c->dupl);
    if (kind == DDA_FRAME_CONTENT && FAILED(hr)) {
        set_err(c, "readback", hr);
        return DDA_ERR;
    }

    if (out) {
        ZeroMemory(out, sizeof(*out));
        out->kind = kind;
        out->present_time = info.LastPresentTime.QuadPart;
        out->accumulated = info.AccumulatedFrames;
        out->cursor = c->cursor;
        if (!c->cursor_valid) {
            // 无形状数据（会话无光标等）：位置仍然有效则给位置，len=0。
            out->cursor.len = 0;
        }
    }
    return kind;
}

const uint8_t *dda_cursor_shape(const void *dda) {
    const dda_ctx *c = (const dda_ctx *)dda;
    if (!c || !c->cursor_valid) return NULL;
    return c->shape;
}

void dda_dims(const void *dda, int32_t *w, int32_t *h) {
    const dda_ctx *c = (const dda_ctx *)dda;
    if (w) *w = c ? c->w : 0;
    if (h) *h = c ? c->h : 0;
}

int32_t dda_format(const void *dda) {
    const dda_ctx *c = (const dda_ctx *)dda;
    if (!c || !c->dupl) return 0;
    DXGI_OUTDUPL_DESC d;
    c->dupl->lpVtbl->GetDesc(c->dupl, &d);
    return (int32_t)d.ModeDesc.Format;
}

void dda_destroy(void *dda) {
    dda_ctx *dc = (dda_ctx *)dda;
    if (dc && dc->secure_mode) {
        secure_gdi_teardown(dc);
        if (dc->hSecure) { CloseDesktop(dc->hSecure); dc->hSecure = NULL; }
        if (dc->hDefault) SetThreadDesktop(dc->hDefault);
        dc->secure_mode = 0;
    }
    dda_ctx *c = (dda_ctx *)dda;
    if (!c) return;
    if (c->dupl) c->dupl->lpVtbl->Release(c->dupl);
    if (c->staging) c->staging->lpVtbl->Release(c->staging);
    if (c->ctx) c->ctx->lpVtbl->Release(c->ctx);
    if (c->dev) c->dev->lpVtbl->Release(c->dev);
    if (c->out1) c->out1->lpVtbl->Release(c->out1);
    free(c->shape);
    free(c);
    if (dda_ctx_global == c) dda_ctx_global = NULL;
}

// ---- 安全桌面 GDI 模式实现 ----

static void secure_gdi_teardown(dda_ctx *c) {
    if (c->hbmp) { DeleteObject(c->hbmp); c->hbmp = NULL; }
    if (c->hdcMem) { DeleteDC(c->hdcMem); c->hdcMem = NULL; }
    if (c->hdcScreen) { ReleaseDC(NULL, c->hdcScreen); c->hdcScreen = NULL; }
    c->dib_bits = NULL;
}

// enter_secure_mode_with 切线程到安全桌面并建 GDI 资源。非 SYSTEM 令牌
// SetThreadDesktop 会失败 → 返回 0（调用方走旧回退路径）。
static int enter_secure_mode_with(dda_ctx *c, HDESK hIn) {
    if (!c->hDefault) c->hDefault = GetThreadDesktop(GetCurrentThreadId());
    if (!SetThreadDesktop(hIn)) { set_err(c, "SetThreadDesktop", (HRESULT)0x80070000 | (HRESULT)GetLastError()); return 0; }
    c->hSecure = hIn; // 所有权归 ctx
    c->secure_mode = 1;
    c->gw = GetSystemMetrics(SM_CXSCREEN);
    c->gh = GetSystemMetrics(SM_CYSCREEN);
    return 1;
}

static void enter_secure_mode(dda_ctx *c) {
    char name[64];
    HDESK hIn = NULL;
    if (input_desktop_name(name, sizeof(name), &hIn) && strcmp(name, "Default") != 0) {
        if (!enter_secure_mode_with(c, hIn)) {
            CloseDesktop(hIn);
        }
    } else if (hIn) {
        CloseDesktop(hIn);
    }
}

// leave_secure_mode 桌面切回 Default：恢复线程桌面、释放 GDI、重建 DXGI。
static void leave_secure_mode(dda_ctx *c) {
    secure_gdi_teardown(c);
    if (c->hSecure) { CloseDesktop(c->hSecure); c->hSecure = NULL; }
    if (c->hDefault) SetThreadDesktop(c->hDefault);
    c->secure_mode = 0;
    do_duplicate(c);
}

// secure_gdi_acquire 安全桌面帧：BitBlt(CAPTUREBLT) → DIB → bgra。
// UAC 对话框多为静止，按 timeout 节流轮询。缓冲不足（dims 变化）时只
// 更新尺寸并返回 TIMEOUT，调用方（Go）重分配后下一帧生效。
static int32_t secure_gdi_acquire(dda_ctx *c, uint8_t *bgra, int32_t cap, int32_t timeout_ms, dda_frame *out) {
    char name[64];
    HDESK hIn = NULL;
    if (input_desktop_name(name, sizeof(name), &hIn)) {
        if (strcmp(name, "Default") == 0) {
            CloseDesktop(hIn);
            leave_secure_mode(c);
            if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_TIMEOUT; }
            return DDA_FRAME_TIMEOUT;
        }
        CloseDesktop(hIn);
    }
    int32_t wait = timeout_ms;
    if (wait < 16) wait = 16;
    if (wait > 100) wait = 100;
    Sleep((DWORD)wait);

    if (!c->hdcScreen) {
        c->gw = GetSystemMetrics(SM_CXSCREEN);
        c->gh = GetSystemMetrics(SM_CYSCREEN);
        c->hdcScreen = GetDC(NULL);
        c->hdcMem = CreateCompatibleDC(c->hdcScreen);
    }
    if (c->gw != GetSystemMetrics(SM_CXSCREEN) || c->gh != GetSystemMetrics(SM_CYSCREEN)) {
        secure_gdi_teardown(c);
        c->gw = GetSystemMetrics(SM_CXSCREEN);
        c->gh = GetSystemMetrics(SM_CYSCREEN);
        c->hdcScreen = GetDC(NULL);
        c->hdcMem = CreateCompatibleDC(c->hdcScreen);
    }
    if (!c->hbmp) {
        BITMAPINFO bi;
        ZeroMemory(&bi, sizeof(bi));
        bi.bmiHeader.biSize = sizeof(BITMAPINFOHEADER);
        bi.bmiHeader.biWidth = c->gw;
        bi.bmiHeader.biHeight = -c->gh; // top-down
        bi.bmiHeader.biPlanes = 1;
        bi.bmiHeader.biBitCount = 32;
        bi.bmiHeader.biCompression = BI_RGB;
        c->hbmp = CreateDIBSection(c->hdcScreen, &bi, DIB_RGB_COLORS, &c->dib_bits, NULL, 0);
        if (c->hbmp) SelectObject(c->hdcMem, c->hbmp);
    }
    if (c->gw != c->w || c->gh != c->h) {
        c->w = c->gw; // 通知调用方新尺寸（Dims 读这里）
        c->h = c->gh;
    }
    if (!c->hbmp || !c->dib_bits) {
        if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_TIMEOUT; }
        return DDA_FRAME_TIMEOUT;
    }
    if ((int32_t)(c->gw) * c->gh * 4 > cap) {
        if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_TIMEOUT; }
        return DDA_FRAME_TIMEOUT; // 缓冲不足：调用方按 Dims 重分配
    }
    BitBlt(c->hdcMem, 0, 0, c->gw, c->gh, c->hdcScreen, 0, 0, SRCCOPY | CAPTUREBLT);
    GdiFlush();
    memcpy(bgra, c->dib_bits, (size_t)c->gw * c->gh * 4);
    if (out) {
        ZeroMemory(out, sizeof(*out));
        out->kind = DDA_FRAME_CONTENT;
        out->present_time = 0;
    }
    return DDA_FRAME_CONTENT;
}
