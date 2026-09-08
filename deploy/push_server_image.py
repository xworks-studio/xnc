# push_server_image.py — 把本地 docker save 的 server 镜像 tar 直推 SRV。
# 为什么是 paramiko：deploy/.env 只有密码凭据（无 SSH key），OpenSSH CLI
# 无法非交互输密码；deploy_srv.py 同款连接方式。
# 用法: py -3 deploy/push_server_image.py <tar 路径>
# 流程: SFTP 上传 /tmp/xnc-server.tar → docker load + tag → compose up -d
#   （不带 --no-pull：SRV compose 版本不认识该旗标；镜像已在本地，
#   默认 missing 拉取策略不会去 GHCR）→ 健康检查 + 清理远端 tar。
import os
import sys

import paramiko

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REMOTE_TAR = "/tmp/xnc-server.tar"
IMAGE = "ghcr.io/xworks-studio/xnc-server:latest"


def load_env():
    env = {}
    with open(os.path.join(ROOT, "deploy", ".env"), encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line.startswith("SRV_") and "=" in line:
                k, v = line.split("=", 1)
                env[k] = v.strip()
    return env


def main():
    tar = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
        os.environ.get("TEMP", "/tmp"), "xnc-server-push.tar")
    if not os.path.exists(tar):
        print(f"missing {tar}; run build-server-local.ps1 first")
        return 1
    env = load_env()

    c = paramiko.SSHClient()
    c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    c.connect(
        env["SRV_HOST"],
        port=int(env.get("SRV_SSH_PORT") or 22),
        username=env["SRV_SSH_USER"],
        password=env.get("SRV_SSH_PASSWORD", ""),
        look_for_keys=False,
        allow_agent=False,
    )
    try:
        print(f"upload {os.path.getsize(tar) // 1048576} MB -> {REMOTE_TAR}")
        sftp = c.open_sftp()
        sftp.put(tar, REMOTE_TAR)
        sftp.close()

        cmd = (
            f"docker load -i {REMOTE_TAR} && docker tag {IMAGE} {IMAGE} && "
            "cd /opt/xnc/deploy && docker compose up -d xnc-server && "
            "sleep 8 && curl -sk https://127.0.0.1/api/health -H 'Host: xnc.app' && "
            f"rm -f {REMOTE_TAR}"
        )
        _, stdout, stderr = c.exec_command(cmd, timeout=300)
        out = stdout.read().decode(errors="replace")
        err = stderr.read().decode(errors="replace")
        rc = stdout.channel.recv_exit_status()
        if out.strip():
            print("--- remote out ---")
            print(out.strip())
        if err.strip():
            print("--- remote stderr ---")
            print(err.strip())
        print("remote exit:", rc)
        return rc
    finally:
        c.close()


if __name__ == "__main__":
    sys.exit(main())
