use std::{
    env, fs,
    path::{Path, PathBuf},
    println,
};

/// Link vcpkg package.（自 rustdesk/libs/scrap build.rs 裁剪：仅保留 Windows
/// x64-windows-static 的 vcpkg 链接逻辑与 libyuv 绑定生成；vpx/aom 绑定与
/// homebrew/linux pkg-config 分支随对应模块一并移除。）
fn link_vcpkg(mut path: PathBuf, name: &str) -> PathBuf {
    let target_os = std::env::var("CARGO_CFG_TARGET_OS").unwrap();
    let target_arch = std::env::var("CARGO_CFG_TARGET_ARCH").unwrap();
    let target_arch = if target_arch == "x86_64" {
        "x64".to_owned()
    } else if target_arch == "loongarch64" {
        "loongarch64".to_owned()
    } else if target_arch == "aarch64" {
        "arm64".to_owned()
    } else {
        "arm".to_owned()
    };
    let mut target = if target_os == "macos" {
        if target_arch == "x64" {
            "x64-osx".to_owned()
        } else {
            format!("{}-{}", target_arch, target_os)
        }
    } else if target_os == "windows" {
        format!("{}-windows-static", target_arch)
    } else {
        format!("{}-{}", target_arch, target_os)
    };
    if target_arch == "x86" {
        target = target.replace("x64", "x86");
    }
    println!("cargo:info={}", target);
    if let Ok(vcpkg_root) = std::env::var("VCPKG_INSTALLED_ROOT") {
        path = vcpkg_root.into();
    } else {
        path.push("installed");
    }
    path.push(target);
    println!(
        "cargo:rustc-link-lib=static={}",
        name.trim_start_matches("lib")
    );
    println!(
        "cargo:rustc-link-search={}",
        path.join("lib").to_str().unwrap()
    );
    let include = path.join("include");
    println!("cargo:include={}", include.display());
    include
}

/// Find package（vcpkg 唯一路径：XNC 构建环境固定 Windows + VCPKG_ROOT）。
fn find_package(name: &str) -> Vec<PathBuf> {
    vec![link_vcpkg(std::env::var("VCPKG_ROOT").unwrap().into(), name)]
}

fn generate_bindings(
    ffi_header: &Path,
    include_paths: &[PathBuf],
    ffi_rs: &Path,
    exact_file: &Path,
    regex: &str,
) {
    let mut b = bindgen::builder()
        .header(ffi_header.to_str().unwrap())
        .allowlist_type(regex)
        .allowlist_var(regex)
        .allowlist_function(regex)
        .rustified_enum(regex)
        .trust_clang_mangling(false)
        .layout_tests(false) // breaks 32/64-bit compat
        .generate_comments(false); // comments have prefix /*!\

    for dir in include_paths {
        b = b.clang_arg(format!("-I{}", dir.display()));
    }

    b.generate().unwrap().write_to_file(ffi_rs).unwrap();
    fs::copy(ffi_rs, exact_file).ok(); // ignore failure
}

fn gen_vcpkg_package(package: &str, ffi_header: &str, generated: &str, regex: &str) {
    let includes = find_package(package);
    let src_dir = env::var_os("CARGO_MANIFEST_DIR").unwrap();
    let src_dir = Path::new(&src_dir);
    let out_dir = env::var_os("OUT_DIR").unwrap();
    let out_dir = Path::new(&out_dir);

    let ffi_header = src_dir.join("src").join("bindings").join(ffi_header);
    println!("rerun-if-changed={}", ffi_header.display());
    for dir in &includes {
        println!("rerun-if-changed={}", dir.display());
    }

    let ffi_rs = out_dir.join(generated);
    let exact_file = src_dir.join("generated").join(generated);
    generate_bindings(&ffi_header, &includes, &ffi_rs, &exact_file, regex);
}

fn main() {
    // in this crate, these are also valid configurations
    println!("cargo:rustc-check-cfg=cfg(dxgi,quartz,x11)");

    let target_os = std::env::var("CARGO_CFG_TARGET_OS").unwrap();

    let target = target_build_utils::TargetInfo::new();
    if target.unwrap().target_pointer_width() != "64" {
        // panic!("Only support 64bit system");
    }
    env::remove_var("CARGO_CFG_TARGET_FEATURE");
    env::set_var("CARGO_CFG_TARGET_FEATURE", "crt-static");

    find_package("libyuv");
    gen_vcpkg_package("libyuv", "yuv_ffi.h", "yuv_ffi.rs", ".*");

    if target_os == "ios" {
        // nothing
    } else if cfg!(windows) {
        // The first choice is Windows because DXGI is amazing.
        println!("cargo:rustc-cfg=dxgi");
    } else {
        panic!("XNC vendored scrap only supports Windows (dxgi)");
    }
}
