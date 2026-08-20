"""SRV 连接测试：密码认证 + 部署前置能力探测。凭据从 deploy/machines.env 读取，不回显。"""
import sys

import paramiko

ENV = {}
for line in open("deploy/machines.env", encoding="utf-8-sig"):
    line = line.strip()
    if not line or line.startswith("#") or "=" not in line:
        continue
    k, v = line.split("=", 1)
    ENV[k.strip()] = v.strip().strip('"').strip("'")

host = ENV.get("SRV_SSH_HOST") or ENV.get("SRV_DOMAIN")
port = int(ENV.get("SRV_SSH_PORT") or 22)
user = ENV["SRV_SSH_USER"]
password = ENV.get("SRV_SSH_PASSWORD", "")
if not host or not user or not password:
    sys.exit("machines.env: SRV host/user/password 不完整")

client = paramiko.SSHClient()
client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
client.connect(host, port=port, username=user, password=password,
               timeout=15, look_for_keys=False, allow_agent=False,
               banner_timeout=15, auth_timeout=15)

CHECKS = [
    ("hostname", "hostname"),
    ("os", "grep PRETTY_NAME /etc/os-release"),
    ("kernel/arch", "uname -r -m"),
    ("docker", "docker --version 2>&1 || echo NO-DOCKER"),
    ("docker compose", "docker compose version 2>&1 || echo NO-COMPOSE"),
    ("ports 80/443 占用", "ss -tln | grep -E ':(80|443)\\b' || echo FREE"),
    ("磁盘 /", "df -h / | tail -1 | awk '{print $2, $4, $5}'"),
    ("内存 MB", "free -m | awk '/Mem:/{print $2, $7}'"),
    ("sudo", "sudo -n true 2>&1 && echo SUDO-NOPASSWD || echo SUDO-NEEDS-PW"),
]

for label, cmd in CHECKS:
    _, out, err = client.exec_command(cmd, timeout=20)
    o, e = out.read().decode().strip(), err.read().decode().strip()
    print(f"[{label}] {o or e or '(empty)'}")

client.close()
print("SRV SSH AUTH: OK")
