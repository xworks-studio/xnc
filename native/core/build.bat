@echo off
rem build.bat - build bin/xnc-core.exe (native/core: XNIP frame + HMAC
rem handshake + console pipe server + log + watchdog) with MSVC. Requires
rem VS2022 (vcvars64). Static CRT (/MT) so nodes need no VC runtime
rem installed. Artifacts go to ..\..\bin (gitignored). Targets:
rem   build.bat            build ..\..\bin\xnc-core.exe
rem   build.bat selftest   build + run bin\xnc-core.exe --selftest
rem (Task 5 folded the old separate selftest exe into xnc-core.exe behind
rem --selftest and removed the temporary selftest_main.cpp.)
setlocal
call "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat" >nul 2>&1
if errorlevel 1 (
  echo [core] vcvars64 not found - install VS2022 Build Tools ^(C++ workload^) 1>&2
  exit /b 1
)
cd /d "%~dp0"

if not exist ..\..\bin mkdir ..\..\bin
cl /nologo /utf-8 /W3 /MT /std:c++17 /EHsc xnc-core.cpp pipe_server.cpp frame.cpp handshake.cpp selftest.cpp /Fe:..\..\bin\xnc-core.exe /Fo:..\..\bin\ /link bcrypt.lib advapi32.lib
if errorlevel 1 exit /b 1

echo [core] built ..\..\bin\xnc-core.exe

if "%~1"=="selftest" (
  ..\..\bin\xnc-core.exe --selftest
  if errorlevel 1 exit /b 1
  echo [core] selftest target ok
)
endlocal
