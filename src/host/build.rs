// build.rs — 五进制版本同源注入的缓存正确性钩子(2026-09-15 规范)。
//
// main.rs 的 host_version() 经 option_env!("XNC_HOST_VERSION") 在编译期
// 取版本号,但 cargo 的构建指纹默认不含环境变量——版本号变了、源码没变
// 时会直接命中陈旧缓存,把旧版本号烤进产物且不报错。rerun-if-env-changed
// 让 XNC_HOST_VERSION 变更强制重编译本 crate。
fn main() {
    println!("cargo:rerun-if-env-changed=XNC_HOST_VERSION");
}
