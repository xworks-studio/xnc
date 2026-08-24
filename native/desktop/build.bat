@echo off
rem build.bat - build bin/xnc-desktop.exe (native/desktop: console-diag
rem process skeleton + Task 3 DXGI capture backend + Task 4 MF software
rem H.264 encoder and BGRA->NV12 conversion + Task 5 FrameCache/pipeline
rem with bitstream shaping and stats.json sidecar + M1-Slice2 Task 2
rem real-time pipe server: subscriber fan-out with on-demand IDR + M1-Slice3
rem Task 1 InputManager (SendInput injection) + CursorManager (GetCursorInfo
rem poll) over 0x0108/0x0109) with MSVC. Requires VS2022 (vcvars64).
rem Static CRT (/MT) so nodes need no VC
rem runtime installed. Shares frame/handshake/log from ..\common (XNIP
rem codec + M0 HMAC handshake; bcrypt.lib for CNG); dxgi_capture.cpp needs
rem d3d11.lib (D3D11CreateDevice), mf_encoder.cpp needs mfplat.lib
rem (MFCreate*) + mfuuid.lib (MF MediaType GUIDs) + ole32.lib
rem (CoCreateInstance/CoInitializeEx); rt_pipe_server.cpp needs bcrypt.lib
rem (handshake HMAC/RNG) + advapi32.lib (SDDL/user SID DACL);
rem rem jpeg_wic.cpp (M2-Slice3 Task 3: --jpeg-single snapshot) needs
rem windowscodecs.lib (WIC imaging factory/JPEG encoder); ole32 already linked;
rem input_manager.cpp/cursor_manager.cpp need user32.lib (SendInput/
rem GetKeyState/GetSystemMetrics/GetCursorInfo/OpenInputDesktop/
rem SetThreadDesktop/CloseDesktop); desktop_watch.cpp (M2-Slice1 T1) uses
rem the same user32 entry points (OpenInputDesktop/
rem GetUserObjectInformationW/CloseDesktop); gdi_capture.cpp (M2-Slice1 T3)
rem needs gdi32.lib (BitBlt/GetDIBits/CreateCompatibleDC/
rem CreateCompatibleBitmap) plus user32 (GetDC/ReleaseDC/
rem GetSystemMetrics); backend_ladder.cpp (M2-Slice1 T3) needs no extra
rem libs; artifacts go to ..\..\bin (gitignored).
rem Targets:
rem   build.bat            build ..\..\bin\xnc-desktop.exe
rem   build.bat selftest   build + run bin\xnc-desktop.exe --selftest
setlocal
call "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat" >nul 2>&1
if errorlevel 1 (
  echo [desktop] vcvars64 not found - install VS2022 Build Tools ^(C++ workload^) 1>&2
  exit /b 1
)
cd /d "%~dp0"

if not exist ..\..\bin mkdir ..\..\bin
cl /nologo /utf-8 /W3 /MT /std:c++17 /EHsc xnc-desktop.cpp desktop_watch.cpp dxgi_capture.cpp gdi_capture.cpp backend_ladder.cpp mf_encoder.cpp nv12.cpp pipeline.cpp rt_pipe_server.cpp input_manager.cpp cursor_manager.cpp jpeg_wic.cpp scaled_capture.cpp desktop_selftest.cpp ..\common\frame.cpp ..\common\handshake.cpp /Fe:..\..\bin\xnc-desktop.exe /Fo:..\..\bin\ /link d3d11.lib dxgi.lib mfplat.lib mfuuid.lib ole32.lib bcrypt.lib advapi32.lib user32.lib gdi32.lib windowscodecs.lib oleaut32.lib
if errorlevel 1 exit /b 1

echo [desktop] built ..\..\bin\xnc-desktop.exe

if "%~1"=="selftest" (
  ..\..\bin\xnc-desktop.exe --selftest
  if errorlevel 1 exit /b 1
  echo [desktop] selftest target ok
)
endlocal
