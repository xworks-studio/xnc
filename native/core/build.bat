@echo off
rem build.bat - build bin/xnc-core-selftest.exe (native/core XNIP frame +
rem HMAC handshake selftest) with MSVC. Requires VS2022 (vcvars64). Links the
rem CRT statically (/MT) so nodes need no VC runtime installed. Artifacts go
rem to ..\..\bin (gitignored). Task 4 scope: frame + handshake + selftest;
rem pipe/log/main arrive in Task 5, which also removes selftest_main.cpp.
setlocal
call "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat" >nul 2>&1
if errorlevel 1 (
  echo [core] vcvars64 not found - install VS2022 Build Tools ^(C++ workload^) 1>&2
  exit /b 1
)
cd /d "%~dp0"

if not exist ..\..\bin mkdir ..\..\bin
cl /nologo /utf-8 /W3 /MT /std:c++17 /EHsc selftest.cpp selftest_main.cpp frame.cpp handshake.cpp /Fe:..\..\bin\xnc-core-selftest.exe /Fo:..\..\bin\ /link bcrypt.lib
if errorlevel 1 exit /b 1

echo [core] built ..\..\bin\xnc-core-selftest.exe
endlocal
