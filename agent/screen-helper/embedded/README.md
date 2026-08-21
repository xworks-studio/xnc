# embedded

`xnc-dda.dll` 的嵌入源目录（已 gitignore）。运行 `../dda/build.bat` 后
真实 DLL 会被拷到这里，随 `go:embed *` 打包进 helper exe；helper 启动时
优先加载 exe 同目录的 DLL，缺失则解压嵌入副本后加载。

目录里只有本 README 时 Go 构建照常通过（DLL 不存在是运行期错误）。
