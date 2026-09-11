#!/usr/bin/env python3
"""setup_relay_caddy.py — relay 主机装配 caddy（TCP443 反代 → relay HTTP 腿）。

用法: py -3 deploy/setup_relay_caddy.py <domain> [--env-prefix RELAY1_]
流程: 确认 caddy → 写 Caddyfile（域名反代 127.0.0.1:8080 + ACME email）→
重启 caddy → 等待 ACME 签发 → 验证 https://<domain>/healthz。
"""
import argparse
import pathlib
import sys
import time

import paramiko

HERE = pathlib.Path(__file__).resolve().parent


def load_env(prefix):
    env = {}
    for line in (HERE / ".env").read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            if k.startswith(prefix):
                env[k[len(prefix):].lower()] = v
    return env


CADDYFILE = """{{
    # UDP443 归 relay 的 WT 腿——禁 h3（caddy 只绑 TCP，避免冲突）。
    servers {{
        protocols h1 h2
    }}
}}

{domain} {{
    reverse_proxy 127.0.0.1:8080
}}
"""


def run(cli, cmd, timeout=120):
    _, o, e = cli.exec_command(cmd, timeout=timeout)
    out = o.read().decode(errors="replace").strip()
    err = e.read().decode(errors="replace").strip()
    return out, err


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("domain")
    ap.add_argument("--env-prefix", default="RELAY1_")
    args = ap.parse_args()

    env = load_env(args.env_prefix)
    if not env.get("ssh_host"):
        sys.exit(f"missing {args.env_prefix}SSH_HOST")

    cli = paramiko.SSHClient()
    cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    cli.connect(env["ssh_host"], port=int(env.get("ssh_port", "22")),
                username=env.get("ssh_user", "root"),
                password=env.get("ssh_password", ""),
                look_for_keys=False, allow_agent=False)
    print(f"[caddy] connected: {env['ssh_user']}@{env['ssh_host']}")

    ver, _ = run(cli, "caddy version 2>/dev/null || echo MISSING")
    if ver == "MISSING":
        print("[caddy] installing from ubuntu repo...")
        out, err = run(cli, "DEBIAN_FRONTEND=noninteractive apt-get install -y caddy 2>&1 | tail -2", timeout=300)
        ver, _ = run(cli, "caddy version 2>/dev/null || echo MISSING")
        if ver == "MISSING":
            sys.exit("caddy install failed")
    print(f"[caddy] version: {ver}")

    sftp = cli.open_sftp()
    with sftp.open("/etc/caddy/Caddyfile", "w") as f:
        f.write(CADDYFILE.format(domain=args.domain))
    sftp.close()
    print(f"[caddy] Caddyfile written ({args.domain} -> 127.0.0.1:8080)")

    out, err = run(cli, "systemctl restart caddy && systemctl is-active caddy")
    print(f"[caddy] service: {out or err}")

    # ACME 签发 + 就绪探测（最长 60s）。
    for i in range(12):
        time.sleep(5)
        code, _ = run(cli, f"curl -s --max-time 5 -o /dev/null -w '%{{http_code}}' https://{args.domain}/healthz")
        print(f"[caddy] https probe try {i+1}: {code}")
        if code == "200":
            print("[caddy] OK — TLS term + proxy verified")
            cli.close()
            return
    _, journal = run(cli, "journalctl -u caddy -n 15 --no-pager")
    print(journal)
    cli.close()
    sys.exit(1)


if __name__ == "__main__":
    main()
