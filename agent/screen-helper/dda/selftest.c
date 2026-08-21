// selftest — dda.c 的独立验收工具（无需 Go/DLL）：帧时间线 + 首帧延迟 +
// 光标频率 + 重建恢复 + 黑帧采样 + 首帧 BMP dump。E5/E6 实验载体。
//
// 用法：dda-selftest.exe [seconds] [dump_count]
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdio.h>
#include <stdlib.h>
#include "dda.h"

static ULONGLONG now_ms(void) { return GetTickCount64(); }

static int write_bmp(const char *path, const uint8_t *bgra, int32_t w, int32_t h) {
    uint8_t hdr[54] = {0};
    DWORD data = (DWORD)(w * h * 4);
    DWORD total = 54 + data;
    hdr[0] = 'B'; hdr[1] = 'M';
    *(DWORD *)(hdr + 2) = total;
    *(DWORD *)(hdr + 10) = 54;
    *(DWORD *)(hdr + 14) = 40;
    *(LONG  *)(hdr + 18) = w;
    *(LONG  *)(hdr + 22) = -h; // 负高度 = top-down
    *(WORD  *)(hdr + 26) = 1;
    *(WORD  *)(hdr + 28) = 32;
    FILE *f = NULL;
    if (fopen_s(&f, path, "wb") != 0 || !f) return -1;
    fwrite(hdr, 1, 54, f);
    fwrite(bgra, 1, data, f);
    fclose(f);
    return 0;
}

static int black_sample(const uint8_t *bgra, size_t len) {
    for (size_t i = 0; i + 3 < len; i += 512) {
        if (bgra[i] | bgra[i+1] | bgra[i+2]) return 0;
    }
    return 1;
}

int main(int argc, char **argv) {
    int seconds = argc > 1 ? atoi(argv[1]) : 6;
    int dumpn   = argc > 2 ? atoi(argv[2]) : 0;
    if (seconds <= 0) seconds = 6;

    printf("dda-selftest: abi=%u\n", dda_abi_version());

    int32_t w = 0, h = 0;
    ULONGLONG t0 = now_ms();
    void *dda = dda_create(&w, &h);
    if (!dda) {
        printf("dda-selftest: CREATE FAILED: %s (after %llums)\n", dda_last_error(), now_ms() - t0);
        return 1;
    }
    printf("dda-selftest: created %dx%d fmt=%d in %llums\n", w, h, dda_format(dda), now_ms() - t0);

    uint8_t *bgra = (uint8_t *)malloc((size_t)w * h * 4);
    if (!bgra) { printf("oom\n"); dda_destroy(dda); return 1; }

    ULONGLONG start = now_ms();
    ULONGLONG first_any = 0, first_content = 0, recreate_at = 0;
    int timeouts = 0, cursor_only = 0, content = 0, black = 0, lost = 0, moves = 0, dumped = 0;
    int last_x = -1, last_y = -1;
    char kinds[8];

    while (now_ms() - start < (ULONGLONG)seconds * 1000) {
        dda_frame f;
        int32_t k = dda_acquire(dda, bgra, (int32_t)((size_t)w*h*4), 100, &f);
        ULONGLONG t = now_ms() - start;
        const char *ks = "?";
        switch (k) {
        case DDA_FRAME_CONTENT: {
            ks = "FRAME";
            content++;
            if (!first_content) first_content = t;
            if (!first_any) first_any = t;
            int blk = black_sample(bgra, (size_t)w * h * 4);
            if (blk) black++;
            if (dumped < dumpn) {
                char path[MAX_PATH];
                _snprintf_s(path, sizeof(path), _TRUNCATE, "dda-frame%02d.bmp", dumped);
                write_bmp(path, bgra, w, h);
                printf("dda-selftest: dumped %s\n", path);
                dumped++;
            }
            printf("t=%llums %s#%d %s present=%llu accum=%u cur=(%d,%d,v=%u) shape=%dx%d t=%d\n",
                   t, ks, content, blk ? "BLACK" : "", (unsigned long long)f.present_time,
                   f.accumulated, f.cursor.x, f.cursor.y, f.cursor.visible,
                   f.cursor.w, f.cursor.h, f.cursor.type);
            break;
        }
        case DDA_FRAME_CURSOR_ONLY:
            ks = "CURSOR";
            cursor_only++;
            if (!first_any) first_any = t;
            if (f.cursor.visible && (f.cursor.x != last_x || f.cursor.y != last_y)) {
                moves++; last_x = f.cursor.x; last_y = f.cursor.y;
            }
            printf("t=%llums %s cur=(%d,%d,v=%u) shape=%dx%d t=%d len=%d\n",
                   t, ks, f.cursor.x, f.cursor.y, f.cursor.visible,
                   f.cursor.w, f.cursor.h, f.cursor.type, f.cursor.len);
            break;
        case DDA_FRAME_TIMEOUT:
            timeouts++;
            ks = "idle";
            break;
        case DDA_FRAME_ACCESS_LOST:
            lost++;
            ks = "LOST";
            printf("t=%llums ACCESS_LOST (auto-recreated)\n", t);
            if (!recreate_at) recreate_at = t;
            break;
        default:
            printf("t=%llums FATAL: %s\n", t, dda_last_error());
            dda_destroy(dda);
            free(bgra);
            return 1;
        }
        (void)ks;
    }

    printf("dda-selftest: summary seconds=%d first_any=%llums first_content=%llums "
           "content=%d cursor_only=%d timeouts=%d black=%d lost=%d moves=%d\n",
           seconds, first_any, first_content, content, cursor_only, timeouts,
           black, lost, moves);
    (void)kinds;
    dda_destroy(dda);
    free(bgra);
    return 0;
}
