#!/bin/sh
# install.sh — XNC Linux 节点安装器（agent + CLI + systemd 单元）。
#
# 用法（在解压后的 tar.gz 目录内）:
#   sudo ./install.sh                 # 安装；重跑 = 升级（幂等，原子换版）
#   sudo ./install.sh --uninstall     # 卸载（默认保留 /var/lib/xnc 身份数据）
#   sudo ./install.sh --uninstall --purge-data   # 卸载并清零状态
#
# 布局: /opt/xnc/versions/<ver>/（bin/xnc-agent bin/xnc systemd/）+ /opt/xnc/current
# 符号链接（原子换版）+ /usr/local/bin/{xnc,xnc-agent} 顶层链接 +
# /var/lib/xnc 状态（0700）+ /etc/systemd/system/xnc-agent.service。
# 回滚: ln -sfn /opt/xnc/versions/<旧版> /opt/xnc/current && systemctl restart xnc-agent
# （保留最近 3 个版本目录，current 指向永不修剪）。
set -eu

PREFIX=/opt/xnc
STATE_DIR=/var/lib/xnc
RUN_DIR=/run/xnc
UNIT_PATH=/etc/systemd/system/xnc-agent.service
SERVICE=xnc-agent
XNC_LINK=/usr/local/bin/xnc
AGENT_LINK=/usr/local/bin/xnc-agent
KEEP_VERSIONS=3

log() { printf '[xnc-install] %s\n' "$*"; }
die() { printf '[xnc-install] ERROR: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "must run as root (sudo ./install.sh)"
command -v systemctl >/dev/null 2>&1 || die "systemd required"

PAYLOAD_DIR=$(cd "$(dirname "$0")" && pwd)

uninstall() {
    purge=0
    [ "${1:-}" = "--purge-data" ] && purge=1
    log "stopping $SERVICE"
    systemctl stop "$SERVICE" 2>/dev/null || true
    systemctl disable "$SERVICE" 2>/dev/null || true
    rm -f "$UNIT_PATH" "$XNC_LINK" "$AGENT_LINK"
    systemctl daemon-reload 2>/dev/null || true
    rm -rf "$PREFIX" "$RUN_DIR"
    if [ "$purge" -eq 1 ]; then
        rm -rf "$STATE_DIR"
        log "uninstalled; state purged ($STATE_DIR)"
    else
        log "uninstalled; state kept at $STATE_DIR (identity/binding preserved for reinstall;"
        log "use --purge-data to wipe, and 'xnc deregister' first if the node is still bound)"
    fi
}

case "${1:-}" in
--uninstall)
    [ $# -gt 2 ] && die "usage: install.sh --uninstall [--purge-data]"
    shift
    uninstall "${1:-}"
    exit 0
    ;;
esac
[ $# -eq 0 ] || die "usage: sudo ./install.sh [--uninstall [--purge-data]]"

[ -f "$PAYLOAD_DIR/bin/xnc-agent" ] || die "payload bin/xnc-agent missing (run from the extracted tarball root)"

# 制品可执行位不可信（Windows bsdtar 打包存 0644；curl|bash / 手工解压都
# 不会先 chmod）：探测版本前先授予。staging 拷贝另有统一 chmod。
chmod 0755 "$PAYLOAD_DIR/bin/xnc-agent" 2>/dev/null || true

# 版本从制品自读（构建期 ldflags 注入；与 agent 自报同源）。
VERSION=$("$PAYLOAD_DIR/bin/xnc-agent" --version)
[ -n "$VERSION" ] || die "cannot read agent version from payload"
log "installing XNC $VERSION"

mkdir -p "$PREFIX/versions"

# 展开新版本目录：先落临时目录再原子 rename，current 永不指向半截内容。
STAGE=$(mktemp -d "$PREFIX/stage.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/bin" "$STAGE/systemd"
cp "$PAYLOAD_DIR/bin/xnc-agent" "$STAGE/bin/xnc-agent"
if [ -f "$PAYLOAD_DIR/bin/xnc" ]; then
    cp "$PAYLOAD_DIR/bin/xnc" "$STAGE/bin/xnc"
fi
if [ -f "$PAYLOAD_DIR/systemd/xnc-agent.service" ]; then
    cp "$PAYLOAD_DIR/systemd/xnc-agent.service" "$STAGE/systemd/"
fi
chmod 0755 "$STAGE/bin"/*
DEST="$PREFIX/versions/$VERSION"
rm -rf "$DEST"
mv "$STAGE" "$DEST"
trap - EXIT

# current 原子切换 + 顶层便捷链接。
ln -sfn "$DEST" "$PREFIX/current"
ln -sfn "$PREFIX/current/bin/xnc" "$XNC_LINK"
ln -sfn "$PREFIX/current/bin/xnc-agent" "$AGENT_LINK"

# 状态目录（identity/binding 由 agent 以 0600/0700 落盘收紧）。
install -d -m 0700 "$STATE_DIR"

# systemd 单元（幂等覆盖）+ 启动。
if [ -f "$DEST/systemd/xnc-agent.service" ]; then
    cp "$DEST/systemd/xnc-agent.service" "$UNIT_PATH"
    systemctl daemon-reload
    systemctl enable "$SERVICE" >/dev/null 2>&1 || true
    if systemctl is-active --quiet "$SERVICE"; then
        log "restarting $SERVICE (upgrade)"
        systemctl restart "$SERVICE"
    else
        systemctl start "$SERVICE"
    fi
else
    log "no systemd unit in payload; service not configured"
fi

# 版本修剪：保留最近 KEEP_VERSIONS 个（名字序≈版本序），current 目标永不删。
# shellcheck disable=SC2012
ls "$PREFIX/versions" | sort -r | awk -v keep="$KEEP_VERSIONS" -v cur="$VERSION" '
    $0 == cur { next }
    { n++; if (n > keep - 1) print }
' | while IFS= read -r v; do
    log "pruning old version $v"
    rm -rf "$PREFIX/versions/$v"
done

log "installed $VERSION (service: $(systemctl is-active "$SERVICE" 2>/dev/null || true))"
log "next: sudo xnc register   # 交互登录选集群；节点秒级上线"
log "logs: journalctl -u xnc-agent -f"
