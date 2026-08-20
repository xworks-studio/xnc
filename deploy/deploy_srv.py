"""XNC SRV 生产部署工具。

凭据读 deploy/machines.env（gitignored）。子命令按需单独执行，`all` 全流程幂等：

    py deploy/deploy_srv.py docker    # 安装 Docker（已装则跳过）
    py deploy/deploy_srv.py push      # 打包 proto/server/deploy 上传 /opt/xnc
    py deploy/deploy_srv.py env       # 远端生成 deploy/.env（已存在则保留），回写管理员凭据到本地 machines.env
    py deploy/deploy_srv.py up        # compose up -d --build
    py deploy/deploy_srv.py verify    # 容器状态 + 栈内健康检查 + 公网 HTTPS health
    py deploy/deploy_srv.py logs [svc]
    py deploy/deploy_srv.py all
"""
import argparse
import io
import secrets
import sys
import tarfile
from pathlib import Path

import paramiko

ROOT = Path(__file__).resolve().parent.parent
REMOTE_ROOT = "/opt/xnc"


def load_env() -> dict:
    env = {}
    for line in (ROOT / "deploy" / "machines.env").read_text(encoding="utf-8-sig").splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip().strip('"').strip("'")
    return env


def connect(env: dict) -> paramiko.SSHClient:
    host = env.get("SRV_SSH_HOST") or env.get("SRV_DOMAIN")
    c = paramiko.SSHClient()
    c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    c.connect(host, port=int(env.get("SRV_SSH_PORT") or 22), username=env["SRV_SSH_USER"],
              password=env.get("SRV_SSH_PASSWORD", ""), look_for_keys=False,
              allow_agent=False, timeout=20)
    return c


def run(c: paramiko.SSHClient, cmd: str, timeout: int = 600) -> tuple[int, str]:
    _, out, err = c.exec_command(cmd, timeout=timeout)
    o, e = out.read().decode(errors="replace"), err.read().decode(errors="replace")
    code = out.channel.recv_exit_status()
    return code, (o + ("\n[stderr]\n" + e if e else "")).strip()


def sh(cmd: str, timeout: int = 600) -> str:
    code, out = run(client, cmd, timeout=timeout)
    print(f"$ {cmd}\n{out}")
    if code != 0:
        sys.exit(f"remote command failed ({code}): {cmd}")
    return out


def cmd_docker():
    code, _ = run(client, "command -v docker")
    if code == 0:
        print("docker already installed:", run(client, "docker --version")[1])
        return
    sh("export DEBIAN_FRONTEND=noninteractive; apt-get update -y")
    sh("export DEBIAN_FRONTEND=noninteractive; apt-get install -y ca-certificates curl")
    sh("install -m 0755 -d /etc/apt/keyrings")
    sh("curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc")
    sh("chmod a+r /etc/apt/keyrings/docker.asc")
    sh(('echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] '
        'https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" '
        '> /etc/apt/sources.list.d/docker.list'))
    sh("export DEBIAN_FRONTEND=noninteractive; apt-get update -y")
    sh("export DEBIAN_FRONTEND=nonactive; apt-get install -y docker-ce docker-ce-cli containerd.io "
       "docker-buildx-plugin docker-compose-plugin".replace("nonactive", "noninteractive"))
    sh("docker --version && docker compose version")


def cmd_push():
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for name in ("proto", "server", "deploy"):
            tf.add(ROOT / name, arcname=name,
                   filter=lambda ti: None if ti.name.endswith((".env", "machines.env")) else ti)
    buf.seek(0)
    sh(f"mkdir -p {REMOTE_ROOT}")
    sftp = client.open_sftp()
    remote_tgz = f"{REMOTE_ROOT}/bundle.tgz"
    with sftp.open(remote_tgz, "wb") as f:
        f.set_pipelined(True)
        while chunk := buf.read(1 << 16):
            f.write(chunk)
    sftp.close()
    sh(f"cd {REMOTE_ROOT} && tar xzf bundle.tgz && rm bundle.tgz && ls -la")
    print(f"pushed proto/ server/ deploy/ -> {REMOTE_ROOT}")


def cmd_env():
    code, out = run(client, f"test -s {REMOTE_ROOT}/deploy/.env && echo EXISTS")
    if code == 0:
        print("remote deploy/.env exists, keeping it")
        return
    env = load_env()
    admin_email = env.get("SRV_ADMIN_EMAIL") or "admin@xnc.app"
    values = {
        "XNC_DOMAIN": env["SRV_DOMAIN"],
        "POSTGRES_PASSWORD": secrets.token_urlsafe(24),
        "XNC_JWT_SECRET": secrets.token_urlsafe(48),
        "XNC_ADMIN_EMAIL": admin_email,
        "XNC_ADMIN_PASSWORD": secrets.token_urlsafe(16),
    }
    body = "\n".join(f"{k}={v}" for k, v in values.items()) + "\n"
    sftp = client.open_sftp()
    with sftp.open(f"{REMOTE_ROOT}/deploy/.env", "w") as f:
        f.write(body)
    sftp.close()
    sh(f"chmod 600 {REMOTE_ROOT}/deploy/.env && wc -l {REMOTE_ROOT}/deploy/.env")
    _record_local_admin(values["XNC_ADMIN_EMAIL"], values["XNC_ADMIN_PASSWORD"])
    print("remote .env written (600); admin credentials recorded to deploy/machines.env")


def _record_local_admin(email: str, password: str):
    p = ROOT / "deploy" / "machines.env"
    lines = p.read_text(encoding="utf-8-sig").splitlines()
    out, seen = [], set()
    for ln in lines:
        if ln.startswith("SRV_ADMIN_EMAIL="):
            out.append(f"SRV_ADMIN_EMAIL={email}"); seen.add("e"); continue
        if ln.startswith("SRV_ADMIN_PASSWORD="):
            out.append(f"SRV_ADMIN_PASSWORD={password}"); seen.add("p"); continue
        out.append(ln)
    if "e" not in seen:
        out.append(f"SRV_ADMIN_EMAIL={email}")
    if "p" not in seen:
        out.append(f"SRV_ADMIN_PASSWORD={password}")
    p.write_text("\n".join(out) + "\n", encoding="utf-8")


def cmd_up():
    sh(f"cd {REMOTE_ROOT} && docker compose -f deploy/docker-compose.yml up -d --build", timeout=900)
    sh(f"cd {REMOTE_ROOT} && docker compose -f deploy/docker-compose.yml ps --format 'table {{{{.Name}}}}\\t{{{{.Status}}}}'")


def cmd_logs(service: str):
    sh(f"cd {REMOTE_ROOT} && docker compose -f deploy/docker-compose.yml logs --tail=60 {service}".rstrip())


def cmd_verify():
    sh(f"cd {REMOTE_ROOT} && docker compose -f deploy/docker-compose.yml ps")
    sh("docker exec $(docker ps -qf name=xnc-server) /xnc-server -healthcheck && echo HEALTHCHECK-OK")
    env = load_env()
    sh(f"sleep 3; curl -sS -m 20 https://{env['SRV_DOMAIN']}/api/health && echo")


COMMANDS = {"docker": cmd_docker, "push": cmd_push, "env": cmd_env, "up": cmd_up, "verify": cmd_verify}


def main():
    global client
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=[*COMMANDS, "all", "logs"])
    ap.add_argument("service", nargs="?", default="")
    args = ap.parse_args()
    client = connect(load_env())
    try:
        if args.cmd == "logs":
            cmd_logs(args.service)
        elif args.cmd == "all":
            for name in ("docker", "push", "env", "up", "verify"):
                print(f"\n===== {name} =====")
                COMMANDS[name]()
        else:
            COMMANDS[args.cmd]()
    finally:
        client.close()


if __name__ == "__main__":
    main()
