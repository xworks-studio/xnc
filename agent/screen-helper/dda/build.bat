@echo off
rem build.bat - build xnc-dda.dll and dda-selftest.exe with MSVC, then stage
rem the DLL into the helper embed dir (Go:embed packs it into the exe; the
rem helper extracts and LoadLibrary's it at runtime).
rem Requires VS2022 (vcvars64). DLL links CRT statically (/MT) so nodes need
rem no VC runtime installed.
setlocal
call "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat" >nul 2>&1
if errorlevel 1 (
  echo [dda] vcvars64 not found - install VS2022 Build Tools ^(C++ workload^) 1>&2
  exit /b 1
)
cd /d "%~dp0"

cl /nologo /utf-8 /O2 /W3 /MT /LD /DXNC_DDA_BUILD dda.c /Fe:xnc-dda.dll /link d3d11.lib dxgi.lib dxguid.lib user32.lib
if errorlevel 1 exit /b 1

cl /nologo /utf-8 /O2 /W3 /MT selftest.c dda.c /Fe:dda-selftest.exe /link d3d11.lib dxgi.lib dxguid.lib user32.lib
if errorlevel 1 exit /b 1

if not exist ..\embedded mkdir ..\embedded
copy /y xnc-dda.dll ..\embedded\xnc-dda.dll >nul
echo [dda] built xnc-dda.dll + dda-selftest.exe, DLL staged to ..\embedded\
endlocal
