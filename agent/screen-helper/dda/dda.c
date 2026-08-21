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

int32_t dda_acquire(void *dda, uint8_t *bgra, int32_t timeout_ms, dda_frame *out) {
    dda_ctx *c = (dda_ctx *)dda;
    if (!c || !c->dupl) return DDA_ERR;

    DXGI_OUTDUPL_FRAME_INFO info;
    ZeroMemory(&info, sizeof(info));
    IDXGIResource *res = NULL;
    HRESULT hr = c->dupl->lpVtbl->AcquireNextFrame(c->dupl, (UINT)timeout_ms, &info, &res);
    if (hr == DXGI_ERROR_WAIT_TIMEOUT) {
        if (out) { ZeroMemory(out, sizeof(*out)); out->kind = DDA_FRAME_TIMEOUT; }
        return DDA_FRAME_TIMEOUT;
    }
    if (hr == DXGI_ERROR_ACCESS_LOST) {
        // 重建失败不升级为致命错误：安全桌面（UAC）活跃期间重建会被拒
        // （E_ACCESSDENIED，实测 SYSTEM 亦然）——返回 ACCESS_LOST 让调用
        // 方按自身节拍继续重试，桌面切回后自然恢复。
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
