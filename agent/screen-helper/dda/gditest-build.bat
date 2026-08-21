@echo off
call "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat" >nul 2>&1
cd /d "%~dp0"
cl /nologo /utf-8 /O2 /W3 /MT gditest.c /Fe:gditest.exe /link user32.lib gdi32.lib
