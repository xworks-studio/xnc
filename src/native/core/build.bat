@echo off
rem build.bat - build bin/xnc-core.exe (native/core: console pipe server +
rem watchdog, sharing XNIP frame + HMAC handshake + log from ..\common)
rem with MSVC. Requires VS2022 (vcvars64). Static CRT (/MT) so nodes need
rem no VC runtime installed. Artifacts go to ..\..\..\bin (gitignored).
rem Targets:
rem   build.bat            build ..\..\..\bin\xnc-core.exe
rem   build.bat selftest   build + run bin\xnc-core.exe --selftest
rem (Task 5 folded the old separate selftest exe into xnc-core.exe behind
rem --selftest and removed the temporary selftest_main.cpp.)
setlocal
rem vcvars64 定位：vswhere（任意 VS2022 版本：Community/Enterprise/Pro/
rem BuildTools——本地为 Community，GitHub runner 为 Enterprise）→ 固定路径兜底。
set "VSVARS="
for /f "usebackq tokens=*" %%i in (`"%ProgramFiles(x86)%\Microsoft Visual Studio\Installer\vswhere.exe" -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath 2^>nul`) do set "VSVARS=%%i\VC\Auxiliary\Build\vcvars64.bat"
if not defined VSVARS set "VSVARS=C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat"
call "%VSVARS%" >nul 2>&1
if errorlevel 1 (
  echo [core] vcvars64 not found - install VS2022 Build Tools ^(C++ workload^) 1>&2
  exit /b 1
)
cd /d "%~dp0"

if not exist ..\..\..\bin mkdir ..\..\..\bin
rem Five-binary version single-source (2026-09-15 spec): installer/build.ps1
rem sets XNC_VERSION before invoking this script; default 0.0.0-dev marks a
rem plain dev build. Passed unquoted (versions carry no spaces); common/
rem version.h stringifies the bare token into a string literal.
if not defined XNC_VERSION set "XNC_VERSION=0.0.0-dev"
set XNC_VERSION_DEFINE=/DXNC_VERSION=%XNC_VERSION%
rem Task 6 adds token_manager.cpp + spawn.cpp (session bridge; wtsapi32 for
rem the WTS console-session queries). M2-Slice1 Task 4 adds wts_monitor.cpp
rem (WTSRegisterSessionNotificationEx also lives in wtsapi32.lib). The prod
rem bootstrap adds secret_file.cpp (--secret-file persistence; the DACL lock
rem uses sddl/advapi32, already linked). spawn-env fix adds userenv.lib
rem (CreateEnvironmentBlock: token-derived child environment).
cl /nologo /utf-8 /W3 /MT /std:c++17 /EHsc /MP %XNC_VERSION_DEFINE% xnc-core.cpp service.cpp pipe_server.cpp wts_monitor.cpp token_manager.cpp spawn.cpp secret_file.cpp selftest.cpp ..\common\frame.cpp ..\common\handshake.cpp /Fe:..\..\..\bin\xnc-core.exe /Fo:..\..\..\bin\ /link bcrypt.lib advapi32.lib wtsapi32.lib user32.lib userenv.lib shell32.lib ole32.lib
if errorlevel 1 exit /b 1

echo [core] built ..\..\..\bin\xnc-core.exe

if "%~1"=="selftest" (
  ..\..\..\bin\xnc-core.exe --selftest
  if errorlevel 1 exit /b 1
  echo [core] selftest target ok
)
endlocal
