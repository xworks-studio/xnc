// ddatest — 最小 C 复刻（ffmpeg 对象链），同时 dump IDXGIOutput1 视图的
// vtable 槽 14..24 函数指针，供与 Go probe 对比。
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <d3d11.h>
#include <dxgi1_2.h>
#include <stdio.h>

int main(void) {
    HRESULT hr;
    ID3D11Device* device = NULL;
    ID3D11DeviceContext* context = NULL;
    hr = D3D11CreateDevice(NULL, D3D_DRIVER_TYPE_HARDWARE, NULL,
        D3D11_CREATE_DEVICE_VIDEO_SUPPORT | D3D11_CREATE_DEVICE_BGRA_SUPPORT,
        NULL, 0, D3D11_SDK_VERSION, &device, NULL, &context);
    printf("device hr=0x%08lX\n", (unsigned long)hr);
    if (FAILED(hr)) return 1;

    IDXGIDevice* dxgiDev = NULL;
    device->lpVtbl->QueryInterface(device, &IID_IDXGIDevice, (void**)&dxgiDev);
    IDXGIAdapter* adapter = NULL;
    dxgiDev->lpVtbl->GetAdapter(dxgiDev, &adapter);
    IDXGIOutput* output = NULL;
    hr = adapter->lpVtbl->EnumOutputs(adapter, 0, &output);
    printf("EnumOutputs hr=0x%08lX\n", (unsigned long)hr);
    if (FAILED(hr)) return 1;
    DXGI_OUTPUT_DESC desc;
    output->lpVtbl->GetDesc(output, &desc);
    printf("attached=%d L=%ld T=%ld R=%ld B=%ld\n", desc.AttachedToDesktop,
        (long)desc.DesktopCoordinates.left, (long)desc.DesktopCoordinates.top,
        (long)desc.DesktopCoordinates.right, (long)desc.DesktopCoordinates.bottom);

    IDXGIOutput1* out1 = NULL;
    hr = output->lpVtbl->QueryInterface(output, &IID_IDXGIOutput1, (void**)&out1);
    printf("QI out1 hr=0x%08lX sameptr=%d\n", (unsigned long)hr, (void*)out1 == (void*)output);

    void*** vt = *(void****)out1;
    printf("vtable=%p\n", (void*)vt);
    for (int i = 14; i <= 24; i++) printf("  slot %2d = %p\n", i, vt[i]);

    IDXGIOutputDuplication* dup = NULL;
    hr = out1->lpVtbl->DuplicateOutput(out1, (IUnknown*)device, &dup);
    printf("DuplicateOutput hr=0x%08lX dup=%p\n", (unsigned long)hr, (void*)dup);
    if (dup) {
        DXGI_OUTDUPL_DESC dd;
        dup->lpVtbl->GetDesc(dup, &dd);
        printf("dup %lux%lu rot=%lu\n",
            (unsigned long)dd.ModeDesc.Width, (unsigned long)dd.ModeDesc.Height,
            (unsigned long)dd.Rotation);
        dup->lpVtbl->Release(dup);
    }
    out1->lpVtbl->Release(out1);
    output->lpVtbl->Release(output);
    adapter->lpVtbl->Release(adapter);
    dxgiDev->lpVtbl->Release(dxgiDev);
    context->lpVtbl->Release(context);
    device->lpVtbl->Release(device);
    return 0;
}
