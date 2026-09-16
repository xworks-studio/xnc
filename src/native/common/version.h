// version.h — 五进制版本同源注入（2026-09-15 规范）。
//
// installer/build.ps1 经 build.bat 以 /DXNC_VERSION=<版本> 编译期传入
//（无引号裸 token，避免 batch/cl 的引号转义泥潭；版本号不含空格）；
// 头文件内两层 stringify 成字符串字面量。等价于 Go 侧 ldflags -X 与
// host 侧 XNC_HOST_VERSION 环境变量。缺省 0.0.0-dev 标识未走安装器构建
// 的开发产物。使用方一律引用 XNC_VERSION_S（service 启动日志、
// --version、--selftest 输出），排障与发版校验（build.ps1 五进制比对）
// 消费。
#ifndef XNC_VERSION
#define XNC_VERSION 0.0.0-dev
#endif
#define XNC_VERSION_STR_(x) #x
#define XNC_VERSION_STR(x) XNC_VERSION_STR_(x)
#define XNC_VERSION_S XNC_VERSION_STR(XNC_VERSION)
