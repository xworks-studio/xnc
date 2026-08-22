@echo off
rem build.bat - build bin/xnc-desktop.exe (native/desktop: console-diag
rem process skeleton + Task 3 DXGI capture backend + Task 4 MF software
rem H.264 encoder and BGRA->NV12 conversion) with MSVC. Requires VS2022
rem (vcvars64). Static CRT (/MT) so nodes need no VC runtime installed.
rem Shares log.h from ..\common; dxgi_capture.cpp needs d3d11.lib
rem (D3D11CreateDevice), mf_encoder.cpp needs mfplat.lib (MFCreate*) +
rem mfuuid.lib (MF MediaType GUIDs) + ole32.lib (CoCreateInstance/
rem CoInitializeEx); artifacts go to ..\..\bin (gitignored).
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
cl /nologo /utf-8 /W3 /MT /std:c++17 /EHsc xnc-desktop.cpp dxgi_capture.cpp mf_encoder.cpp nv12.cpp desktop_selftest.cpp /Fe:..\..\bin\xnc-desktop.exe /Fo:..\..\bin\ /link d3d11.lib mfplat.lib mfuuid.lib ole32.lib
if errorlevel 1 exit /b 1

echo [desktop] built ..\..\bin\xnc-desktop.exe

if "%~1"=="selftest" (
  ..\..\bin\xnc-desktop.exe --selftest
  if errorlevel 1 exit /b 1
  echo [desktop] selftest target ok
)
endlocal
