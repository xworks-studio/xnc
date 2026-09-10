#pragma once

#include <minwindef.h>
#include <winioctl.h>
#include <guiddef.h>

#define IOCTL_CHANGER_IDD_PLUG_IN             CTL_CODE(IOCTL_CHANGER_BASE, \
                                                       0x1001, \
                                                       METHOD_BUFFERED, \
                                                       FILE_READ_ACCESS | FILE_WRITE_ACCESS)
#define IOCTL_CHANGER_IDD_PLUG_OUT            CTL_CODE(IOCTL_CHANGER_BASE, \
                                                       0x1002, \
                                                       METHOD_BUFFERED, \
                                                       FILE_READ_ACCESS | FILE_WRITE_ACCESS)
#define IOCTL_CHANGER_IDD_UPDATE_MONITOR_MODE CTL_CODE(IOCTL_CHANGER_BASE, \
                                                       0x1003, \
                                                       METHOD_BUFFERED, \
                                                       FILE_READ_ACCESS | FILE_WRITE_ACCESS)
// XNC 增补（上游无此接口）：查询各连接器插拔/激活状态。激活 = OS 已分配
// swapchain（显示器被点亮）——agent 运行于 session 0，GDI/DXGI 枚举均不可
// 用（2026-09-10 XIAOXIN 实测），此接口是会话无关的唯一权威信号。
#define IOCTL_CHANGER_IDD_GET_STATUS           CTL_CODE(IOCTL_CHANGER_BASE, \
                                                       0x1004, \
                                                       METHOD_BUFFERED, \
                                                       FILE_READ_ACCESS | FILE_WRITE_ACCESS)

// 连接器上限（Driver.h m_sMaxMonitorCount 同源；保持小值即可）。
#define IDD_MAX_MONITOR_COUNT 10


#define STATUS_ERROR_ADAPTER_NOT_INIT      (3 << 30) + 11
//#define STATUS_ERROR_IO_CTL_GET_INPUT    (3 << 30) + 21
//#define STATUS_ERROR_IO_CTL_GET_OUTPUT   (3 << 30) + 22
#define STATUS_ERROR_MONITOR_EXISTS        (3 << 30) + 51
#define STATUS_ERROR_MONITOR_NOT_EXISTS    (3 << 30) + 52
#define STATUS_ERROR_MONITOR_INVALID_PARAM (3 << 30) + 53
#define STATUS_ERROR_MONITOR_OOM           (3 << 30) + 54
#define STATUS_ERROR_INDEX_OOR             (3 << 30) + 55

#define MONITOR_EDID_MOD_XNC_VIRTUAL_DISPLAY 0

typedef struct _CtlPlugIn {
    UINT ConnectorIndex;
    UINT MonitorEDID;
    GUID ContainerId;
} CtlPlugIn, *PCtlPlugIn;

typedef struct _CtlPlugOut {
    UINT ConnectorIndex;
} CtlPlugOut, *PCtlPlugOut;

typedef struct _CtlMonitorModes {
    UINT ConnectorIndex;
    UINT ModeCount;
    struct {
        DWORD Width;
        DWORD Height;
        DWORD Sync;
    } Modes[1];
} CtlMonitorModes, *PCtlMonitorModes;

typedef struct _CtlMonitorStatus {
    UINT ConnectorCount;
    struct {
        BOOL Plugged;
        BOOL Active;
    } Connectors[IDD_MAX_MONITOR_COUNT];
} CtlMonitorStatus, *PCtlMonitorStatus;


#define SYMBOLIC_LINK_NAME L"\\Device\\XncIdd"

