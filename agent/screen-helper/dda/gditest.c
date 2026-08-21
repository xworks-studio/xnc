// gditest — 安全桌面 GDI 捕获验证（RustDesk 路数复刻，实验工具）。
//
// 机制：SYSTEM 令牌 + SetThreadDesktop(winlogon/当前输入桌面) +
// BitBlt(CAPTUREBLT) 到 DIB。轮询等待输入桌面离开 Default（UAC 激活），
// 捕获一帧 BMP 落盘，随后恢复线程桌面。
//
// 用法：gditest.exe <wait_seconds> <out.bmp>
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

static HDESK hDefault;

static int write_bmp(const char *path, const uint8_t *bgra, int32_t w, int32_t h) {
    uint8_t hdr[54] = {0};
    *(DWORD *)(hdr + 2) = 54 + (DWORD)(w * h * 4);
    *(DWORD *)(hdr + 10) = 54;
    *(DWORD *)(hdr + 14) = 40;
    *(LONG *)(hdr + 18) = w;
    *(LONG *)(hdr + 22) = -h; // top-down
    *(WORD *)(hdr + 26) = 1;
    *(WORD *)(hdr + 28) = 32;
    FILE *f = NULL;
    if (fopen_s(&f, path, "wb") != 0 || !f) return -1;
    fwrite(hdr, 1, 54, f);
    fwrite(bgra, 1, (size_t)w * h * 4, f);
    fclose(f);
    return 0;
}

int main(int argc, char **argv) {
    int wait_s = argc > 1 ? atoi(argv[1]) : 20;
    const char *out = argc > 2 ? argv[2] : "gditest-uac.bmp";

    hDefault = GetThreadDesktop(GetCurrentThreadId());

    for (int t = 0; t < wait_s * 10; t++) {
        // SYSTEM 可打开当前输入桌面；名字非 Default = 安全桌面激活。
        HDESK hIn = OpenInputDesktop(0, FALSE, GENERIC_READ);
        if (!hIn) {
            printf("t=%dms OpenInputDesktop failed gle=%lu\n", t * 100, GetLastError());
            Sleep(100);
            continue;
        }
        char name[64] = {0};
        DWORD need = 0;
        GetUserObjectInformationA(hIn, UOI_NAME, name, sizeof(name), &need);
        BOOL secure = strcmp(name, "Default") != 0;
        if (!secure) {
            CloseDesktop(hIn);
            Sleep(100);
            continue;
        }
        printf("t=%dms secure desktop \"%s\" active -> SetThreadDesktop\n", t * 100, name);
        if (!SetThreadDesktop(hIn)) {
            printf("SetThreadDesktop failed gle=%lu\n", GetLastError());
            CloseDesktop(hIn);
            break;
        }

        // GDI 捕获：BitBlt(CAPTUREBLT) 到 32bpp DIB。
        int w = GetSystemMetrics(SM_CXSCREEN), h = GetSystemMetrics(SM_CYSCREEN);
        printf("capturing %dx%d\n", w, h);
        HDC hdcScreen = GetDC(NULL);
        HDC hdcMem = CreateCompatibleDC(hdcScreen);
        BITMAPINFO bi = {0};
        bi.bmiHeader.biSize = sizeof(BITMAPINFOHEADER);
        bi.bmiHeader.biWidth = w;
        bi.bmiHeader.biHeight = -h; // top-down
        bi.bmiHeader.biPlanes = 1;
        bi.bmiHeader.biBitCount = 32;
        bi.bmiHeader.biCompression = BI_RGB;
        void *bits = NULL;
        HBITMAP hbmp = CreateDIBSection(hdcScreen, &bi, DIB_RGB_COLORS, &bits, NULL, 0);
        if (!hbmp || !bits) {
            printf("CreateDIBSection failed gle=%lu\n", GetLastError());
        } else {
            HGDIOBJ old = SelectObject(hdcMem, hbmp);
            // 等安全桌面完全呈现
            Sleep(700);
            BOOL ok = BitBlt(hdcMem, 0, 0, w, h, hdcScreen, 0, 0, SRCCOPY | CAPTUREBLT);
            printf("BitBlt %s gle=%lu\n", ok ? "ok" : "FAILED", GetLastError());
            if (ok && write_bmp(out, (const uint8_t *)bits, w, h) == 0) {
                printf("dumped %s\n", out);
            }
            SelectObject(hdcMem, old);
            DeleteObject(hbmp);
        }
        DeleteDC(hdcMem);
        ReleaseDC(NULL, hdcScreen);

        SetThreadDesktop(hDefault); // 恢复
        CloseDesktop(hIn);
        break;
    }
    printf("gditest done\n");
    return 0;
}
