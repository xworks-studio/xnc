/* main.c - non-interactive controller for XncIdd (XWorks XNC Virtual Display).
   Dev/test tooling mirroring upstream RustDeskIddApp; the production agent
   (Go, src/agent/display) issues the same IOCTLs in-process.

   usage: XncIddCtl install <inf> | uninstall <inf> | create | destroy
          | plug [edidIdx] | unplug [connIdx] | created | modes | enum        */
#include <stdio.h>
#include <windows.h>
#include "./IddController.h"

#pragma comment(lib, "swdevice.lib")
#pragma comment(lib, "Cfgmgr32.lib")
#pragma comment(lib, "Setupapi.lib")
#pragma comment(lib, "Newdev.lib")
#pragma comment(lib, "ole32.lib")
#pragma comment(lib, "user32.lib")

static void EnumDisplays(void)
{
    DISPLAY_DEVICEW dd;
    DWORD i = 0;
    printf("== displays ==\n");
    for (;;)
    {
        ZeroMemory(&dd, sizeof(dd));
        dd.cb = sizeof(dd);
        if (!EnumDisplayDevicesW(NULL, i, &dd, 0)) break;
        printf("[%u] name=%ls string=%ls state=0x%x active=%s primary=%s attached=%s\n",
               i, dd.DeviceName, dd.DeviceString, dd.StateFlags,
               (dd.StateFlags & DISPLAY_DEVICE_ACTIVE) ? "yes" : "no",
               (dd.StateFlags & DISPLAY_DEVICE_PRIMARY_DEVICE) ? "yes" : "no",
               (dd.StateFlags & DISPLAY_DEVICE_ATTACHED_TO_DESKTOP) ? "yes" : "no");
        i++;
    }
    printf("total=%u\n", i);
}

static UINT ParseUint(const wchar_t* s, UINT def)
{
    UINT v = 0;
    if (s == NULL || swscanf_s(s, L"%u", &v) != 1) return def;
    return v;
}

int wmain(int argc, wchar_t* argv[])
{
    SetPrintErrMsg(TRUE);
    if (argc < 2)
    {
        printf("usage: XncIddCtl install <inf> | uninstall <inf> | create | destroy | plug [edid] | unplug [conn] | created | modes | enum\n");
        return 2;
    }

    if (0 == wcscmp(argv[1], L"install"))
    {
        if (argc < 3) return 2;
        BOOL reboot = FALSE;
        BOOL ok = InstallUpdate(argv[2], &reboot);
        printf("install %s rebootRequired=%d\n", ok ? "OK" : "FAIL", reboot);
        if (!ok) printf("%s\n", GetLastMsg());
        return ok ? 0 : 1;
    }
    if (0 == wcscmp(argv[1], L"uninstall"))
    {
        if (argc < 3) return 2;
        BOOL reboot = FALSE;
        BOOL ok = Uninstall(argv[2], &reboot);
        printf("uninstall %s rebootRequired=%d\n", ok ? "OK" : "FAIL", reboot);
        if (!ok) printf("%s\n", GetLastMsg());
        return ok ? 0 : 1;
    }
    if (0 == wcscmp(argv[1], L"create"))
    {
        /* ParentPresent: device survives this process; the production agent
           uses SWDeviceLifetimeHandle so an agent crash auto-removes it. */
        SW_DEVICE_LIFETIME lt = SWDeviceLifetimeParentPresent;
        HSWDEVICE h = NULL;
        BOOL ok = DeviceCreateWithLifetime(&lt, &h);
        printf("create %s\n", ok ? "OK" : "FAIL");
        if (!ok) printf("%s\n", GetLastMsg());
        if (h) SwDeviceClose(h);
        return ok ? 0 : 1;
    }
    if (0 == wcscmp(argv[1], L"destroy"))
    {
        DeviceClose(NULL);
        printf("destroy done\n");
        return 0;
    }
    if (0 == wcscmp(argv[1], L"plug"))
    {
        UINT edid = ParseUint(argc > 2 ? argv[2] : NULL, 0);
        UINT idx = ParseUint(argc > 3 ? argv[3] : NULL, 0);
        BOOL ok = MonitorPlugIn(idx, edid, 10);
        printf("plug %s (conn=%u edid=%u)\n", ok ? "OK" : "FAIL", idx, edid);
        if (!ok) printf("%s\n", GetLastMsg());
        else
        {
            /* mirror upstream app: refresh the supported modes after arrival */
            MonitorMode modes[2] = { { 1920, 1080, 60 }, { 1024, 768, 60 } };
            MonitorModesUpdate(idx, 2, modes);
        }
        return ok ? 0 : 1;
    }
    if (0 == wcscmp(argv[1], L"unplug"))
    {
        UINT idx = ParseUint(argc > 2 ? argv[2] : NULL, 0);
        BOOL ok = MonitorPlugOut(idx);
        printf("unplug %s (conn=%u)\n", ok ? "OK" : "FAIL", idx);
        if (!ok) printf("%s\n", GetLastMsg());
        return ok ? 0 : 1;
    }
    if (0 == wcscmp(argv[1], L"created"))
    {
        BOOL created = FALSE;
        BOOL ok = IsDeviceCreated(&created);
        printf("created %s device=%s\n", ok ? "OK" : "FAIL", created ? "yes" : "no");
        return (ok && created) ? 0 : 1;
    }
    if (0 == wcscmp(argv[1], L"modes"))
    {
        UINT idx = ParseUint(argc > 2 ? argv[2] : NULL, 0);
        MonitorMode modes[2] = { { 1920, 1080, 60 }, { 1024, 768, 60 } };
        BOOL ok = MonitorModesUpdate(idx, 2, modes);
        printf("modes %s (conn=%u)\n", ok ? "OK" : "FAIL", idx);
        if (!ok) printf("%s\n", GetLastMsg());
        return ok ? 0 : 1;
    }
    if (0 == wcscmp(argv[1], L"enum"))
    {
        EnumDisplays();
        return 0;
    }
    return 2;
}
