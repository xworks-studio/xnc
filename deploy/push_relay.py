#!/usr/bin/env python3
"""push_relay.py — 把 xnc-relay 部署到中继机(relay-plane T9)。

流程:读 deploy/.env 的 RELAY1_* 键 → SFTP 上传二进制 → 渲染 systemd 单元
(模式抄 push_server_image.py:paramiko 密码认证)→ 安装/重启 → journalctl
验收。凭据绝不入 argv 之外的回显面(.env 唯一源)。

用法:
  py -3 deploy/push_relay.py <local-binary> [--server wss://xnc.app/api/relay/connect]
      [--public-host 1.2.3.4] [--region cn-hangzhou] [--max-sessions 100]
      [--max-mbps-out 500] [--unit deploy/xnc-relay.service]
"""
import argparse
import pathlib
import sys
import time

import paramiko

HERE = pathlib.Path(__file__).resolve().parent


def load_env(relay_prefix="RELAY1_"):
    env = {}
    for line in (HERE / ".env").read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            if k.startswith(relay_prefix):
                env[k[len(relay_prefix):].lower()] = v
    return env


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--server", default="wss://xnc.app/api/relay/connect")
    ap.add_argument("--public-host", required=True)
    ap.add_argument("--region", default="")
    ap.add_argument("--allow-origin", default="https://xnc.app")
    ap.add_argument("--max-sessions", default="100")
    ap.add_argument("--max-mbps-out", default="500")
    ap.add_argument("--unit", default=str(HERE / "xnc-relay.service"))
    # 多 relay 凭据键前缀（deploy/.env 的 RELAY1_/RELAY2_…；2026-09-11）。
    ap.add_argument("--env-prefix", default="RELAY1_")
    args = ap.parse_args()

    env = load_env(args.env_prefix)
    if not env.get("ssh_host"):
        sys.exit(f"deploy/.env missing {args.env_prefix}SSH_HOST")
    host = env["ssh_host"]
    user = env.get("ssh_user", "root")
    password = env.get("ssh_password", "")

    cli = paramiko.SSHClient()
    cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    cli.connect(host, port=int(env.get("ssh_port", "22")), username=user,
                password=password, timeout=20)
    print(f"[relay] connected: {user}@{host}")

    # ① 上传二进制。
    sftp = cli.open_sftp()
    sftp.put(args.binary, "/usr/local/bin/xnc-relay.new")
    sftp.chmod("/usr/local/bin/xnc-relay.new", 0o755)
    run(cli, "mv /usr/local/bin/xnc-relay.new /usr/local/bin/xnc-relay")
    sftp.close()
    print("[relay] binary installed")

    # ② 渲染单元。
    unit = pathlib.Path(args.unit).read_text(encoding="utf-8")
    unit = (unit.replace("{{SERVER_URL}}", args.server)
                .replace("{{PUBLIC_HOST}}", args.public_host)
                .replace("{{REGION}}", args.region)
                .replace("{{ALLOW_ORIGIN}}", args.allow_origin)
                .replace("{{MAX_SESSIONS}}", args.max_sessions)
                .replace("{{MAX_MBPS_OUT}}", args.max_mbps_out))
    sftp = cli.open_sftp()
    with sftp.open("/etc/systemd/system/xnc-relay.service", "w") as f:
        f.write(unit)
    sftp.close()
    print("[relay] unit installed")

    # ③ 起服务。
    run(cli, "systemctl daemon-reload && systemctl enable --now xnc-relay")
    run(cli, "systemctl restart xnc-relay")
    time.sleep(2)
    # ④ 验收:active + 最近日志(确认注册/认证走到哪一步)。
    _, out, _ = cli.exec_command("systemctl is-active xnc-relay")
    active = out.read().decode().strip()
    print(f"[relay] service: {active}")
    _, out, _ = cli.exec_command("journalctl -u xnc-relay -n 15 --no-pager")
    print(out.read().decode(errors="replace"))
    cli.close()
    if active != "active":
        sys.exit(1)
    print("[relay] deploy OK - relay will register to server (pending/active "
          "per allowlist; approve via PATCH /api/admin/relays/{id})")


def run(cli, cmd):
    _, out, err = cli.exec_command(cmd)
    o, e = out.read().decode(errors="replace"), err.read().decode(errors="replace")
    if o.strip():
        print(f"[relay] {o.strip()}")
    if e.strip():
        print(f"[relay:stderr] {e.strip()}")


if __name__ == "__main__":
    main()
