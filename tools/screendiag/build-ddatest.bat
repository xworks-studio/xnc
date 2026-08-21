@echo off
call "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat" >nul 2>&1
cd /d "%~dp0"
cl /nologo /O2 ddatest.c /Fe:ddatest.exe /link d3d11.lib dxgi.lib dxguid.lib user32.lib
