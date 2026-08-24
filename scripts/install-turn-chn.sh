#!/usr/bin/env bash
# install-turn-chn.sh — 境内 TURN 媒体中转节点部署脚本(免备案,纯 IP)。
# 用法(境内 Ubuntu 机器,root):
#   bash install-turn-chn.sh <公网IP> [realm] [username] [password]
# 例:bash install-turn-chn.sh 47.100.10.10 xnc.app xncdev xncdev-secret
# 产物:coturn(systemd turnserver)+ /etc/turnserver.conf + UFW 放行 +
#       本机 STUN 自检。完成后把本机公网 IP 加入主站 .env 的 XNC_TURN_POOL。
# 凭据要求:password 须与主站 deploy/.env 的 XNC_TURN_CREDENTIAL 同值
# (池内 coturn 与 server 共凭据);未传时读 TURN_USER_PASSWORD,再缺省
# xncdev-secret(与 CREDENTIAL 默认一致)。
set -euo pipefail

PUB_IP="${1:?usage: install-turn-chn.sh <public-ip> [realm] [user] [pass]}"
REALM="${2:-xnc.app}"
USER="${3:-xncdev}"
PASS="${4:-${TURN_USER_PASSWORD:-xncdev-secret}}"

echo "== [1/5] install coturn"
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq coturn

echo "== [2/5] write /etc/turnserver.conf"
cat > /etc/turnserver.conf <<EOF
# XNC 境内媒体中转节点($PUB_IP)。与主站 TURN 池共享 realm/凭据;
# 纯 IP 寻址(免备案)。relay 段需与安全组一致。
realm=${REALM}
fingerprint
lt-cred-mech
user=${USER}:${PASS}
listening-port=3478
listening-ip=0.0.0.0
relay-ip=0.0.0.0
EXT_IP="${PUB_IP}/$(ip -4 route get 1 2>/dev/null | awk '{print $7; exit}')"
# 注意:external-ip 必须为 公网/内网 双值(NAT 后标准配置,缺内网会 403 Forbidden IP)
external-ip=${EXT_IP}
min-port=49160
max-port=49200
no-cli
no-tls
no-dtls
EOF
chmod 644 /etc/turnserver.conf

echo "== [3/5] enable + start"
sed -i 's/^#TURNSERVER_ENABLED=1/TURNSERVER_ENABLED=1/' /etc/default/coturn || true
systemctl enable coturn
systemctl restart coturn
sleep 2
systemctl --no-pager status coturn | head -3 || true

echo "== [4/5] firewall (UFW)"
ufw allow 3478/tcp >/dev/null 2>&1 || true
ufw allow 3478/udp >/dev/null 2>&1 || true
ufw allow 49160:49200/udp >/dev/null 2>&1 || true
echo "ufw rules:"
ufw status | grep -E "3478|49160" || true

echo "== [5/5] STUN self-check (127.0.0.1)"
python3 - <<'PY' || { echo "STUN self-check FAILED"; exit 1; }
import socket, struct, os
m = struct.pack(">HHI", 1, 0, 0x2112A442) + os.urandom(12)
for proto, addr in (("udp", ("127.0.0.1", 3478)), ("tcp", ("127.0.0.1", 3478))):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM if proto == "udp" else socket.SOCK_STREAM)
    s.settimeout(3)
    if proto == "tcp": s.connect(addr)
    s.sendto(m, addr) if proto == "udp" else s.send(m)
    r = s.recv(64) if proto == "tcp" else s.recvfrom(64)[0]
    ok = len(r) >= 8 and struct.unpack(">H", r[:2])[0] == 0x0101
    print(f"STUN {proto}: {'ok' if ok else 'BAD'}")
    if not ok: raise SystemExit(1)
PY

echo "== done =="
echo "加入主站 TURN 池:.env 的 XNC_TURN_POOL 追加 ${PUB_IP}:3478"
