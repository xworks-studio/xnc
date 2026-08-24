"""XNC SRV 生产部署工具。

凭据读 deploy/.env（gitignored）。子命令按需单独执行，`all` 全流程幂等：

    py deploy/deploy_srv.py docker    # 安装 Docker（已装则跳过）
    py deploy/deploy_srv.py push      # 打包 proto/server/deploy 上传 /opt/xnc
    py deploy/deploy_srv.py env       # 远端生成 deploy/.env（已存在则保留），回写管理员凭据到本地 deploy/.env
    py deploy/deploy_srv.py up        # 渲染 turnserver.conf + 清同名孤儿,compose up -d --build(Conflict 时 rm 重试一次)
    py deploy/deploy_srv.py verify    # 容器状态 + 栈内健康检查 + 公网 HTTPS health + 本机 STUN 自检(UDP/TCP)
    py deploy/deploy_srv.py logs [svc]
    py deploy/deploy_srv.py all

版本单一来源：本地 deploy/.env 设 XNC_VERSION（与 agent bundle 版本一致）时，
`env` 会写入远端 .env，compose build 经 ARG 注入 server 版本；未设回落 0.0.0-dev。
"""
import argparse
import io
import re
import secrets
import shutil
import subprocess
import sys
import tarfile
from pathlib import Path

import paramiko

ROOT = Path(__file__).resolve().parent.parent
REMOTE_ROOT = "/opt/xnc"


def load_env() -> dict:
    env = {}
    for line in (ROOT / "deploy" / ".env").read_text(encoding="utf-8-sig").splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip().strip('"').strip("'")
    return env


def _parse_env_text(raw: str) -> dict:
    env = {}
    for line in raw.splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip().strip('"').strip("'")
    return env


# turnserver.conf 模板插值:coturn 配置文件本身不支持 ${VAR} 展开(2026-08-24
# retro Task 6 调研),故由本工具在 `up` 前用 deploy/.env(compose 插值同源)
# 渲染。支持 ${VAR} 与 ${VAR:-default};渲染后为空的键值行整行丢弃(如
# relay-ip=),避免 coturn 拿到空值。
_TURN_VAR_RE = re.compile(r"\$\{([A-Z][A-Z0-9_]*)(?::-([^}]*))?\}")


def _render_turn_conf(tmpl: str, env: dict) -> str:
    def sub(m: re.Match) -> str:
        name, default = m.group(1), m.group(2)
        v = env.get(name)
        if v is not None and v != "":
            return v
        return default if default is not None else ""

    out = []
    for line in tmpl.splitlines():
        if line.strip().startswith("#"):
            out.append(line)  # 注释不参与插值(保留 ${...} 模板说明)
            continue
        rendered = _TURN_VAR_RE.sub(sub, line)
        if not rendered.strip():
            continue
        if "=" in rendered:
            k, _, v = rendered.partition("=")
            if k.strip() and not v.strip():
                continue  # 值渲染为空 → 整行丢弃
        out.append(rendered)
    return "\n".join(out) + "\n"


def _write_remote_turn_conf():
    """远端渲染 deploy/turnserver.conf(模板 + deploy/.env,单一事实;
    幂等:已渲染的静态文件原样写出)。"""
    sftp = client.open_sftp()
    try:
        try:
            with sftp.open(f"{REMOTE_ROOT}/deploy/.env") as f:
                env = _parse_env_text(f.read().decode(errors="replace"))
        except OSError:
            sys.exit(f"{REMOTE_ROOT}/deploy/.env missing — run `env` first")
        try:
            with sftp.open(f"{REMOTE_ROOT}/deploy/turnserver.conf") as f:
                tmpl = f.read().decode(errors="replace")
        except OSError:
            sys.exit(f"{REMOTE_ROOT}/deploy/turnserver.conf missing — run `push` first")
    finally:
        sftp.close()
    body = _render_turn_conf(tmpl, env)
    sftp = client.open_sftp()
    try:
        with sftp.open(f"{REMOTE_ROOT}/deploy/turnserver.conf", "w") as f:
            f.write(body)
        sftp.chmod(f"{REMOTE_ROOT}/deploy/turnserver.conf", 0o644)  # coturn 容器以 nobody 运行,0600 root 读不了(2026-08-24 生产事故)
    finally:
        sftp.close()
    print("rendered deploy/turnserver.conf from deploy/.env (template vars resolved)")


def _remote_py(script: str) -> str:
    """包装一段 python3 脚本供远端执行(远端 shell heredoc;需远端有
    python3 —— Ubuntu Server 自带)。"""
    return "python3 - <<'XNC_PY_EOF'\n" + script + "\nXNC_PY_EOF"


# STUN binding 自检(UDP + TCP,127.0.0.1:3478,docker-proxy → coturn 容器):
# 发 20B binding request,期望回包 type=0x0101(binding response)且携带
# magic cookie;长度 ≥ 20B(实际按 coturn 应答打印,通常 ~40B)。
_STUN_PROBE = r'''
import socket, struct, sys

TID = b"\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c"
REQ = struct.pack(">HHI12s", 0x0001, 0, 0x2112A442, TID)


def probe(kind, sock_type):
    s = socket.socket(socket.AF_INET, sock_type)
    s.settimeout(4)
    try:
        if sock_type == socket.SOCK_STREAM:
            s.connect(("127.0.0.1", 3478))
            s.sendall(REQ)
        else:
            s.sendto(REQ, ("127.0.0.1", 3478))
        data = s.recv(512)
        if len(data) < 20:
            print(f"STUN {kind}: fail (short response {len(data)}B)")
            return 1
        mtype = struct.unpack(">H", data[:2])[0]
        ok = mtype == 0x0101 and data[4:8] == b"\x21\x12\xa4\x42"
        print(f"STUN {kind}: {'ok' if ok else 'fail'} ({len(data)}B, type=0x{mtype:04x})")
        return 0 if ok else 1
    except Exception as e:
        print(f"STUN {kind}: fail ({e})")
        return 1
    finally:
        s.close()


sys.exit(0 if probe("udp", socket.SOCK_DGRAM) + probe("tcp", socket.SOCK_STREAM) == 0 else 1)
'''


# up 前清同名孤儿:名字命中本 compose 项目(deploy-<svc>-1)但项目 label
# 不是 deploy 的残留容器(2026-08-24 retro P2#10:d00097cd9d8c_* 旧世代
# 容器曾导致 rebuild 报名字冲突)。
# 渲染注意:模板含 ${svc} 等 shell 变量,不能用 str.format(root=...)——
# .format 会把 ${svc} 当字段并抛 KeyError(2026-08-24 retro 复现),故
# 用 _orphan_prune_script 的 {root} 纯文本替换渲染。
_ORPHAN_PRUNE = r'''
cd {root} && for svc in $(docker compose -f deploy/docker-compose.yml config --services); do
  name="deploy-${svc}-1"
  if docker inspect -f '{{.State.Status}}' "$name" >/dev/null 2>&1; then
    proj=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$name" 2>/dev/null)
    if [ "$proj" != "deploy" ]; then
      echo "prune orphan container: $name (project='$proj')"
      docker rm -f "$name" && echo "  removed"
    fi
  fi
done
'''


def _orphan_prune_script(root: str) -> str:
    """渲染孤儿清理脚本并做健全性检查:只做 {root} 文本替换(绝不做
    str.format),渲染后断言 root 已展开、${svc} shell 插值保留、无模板
    大括号残留;bash 可用时再 bash -n 语法检查。任一失败即退出,不带病
    上远端。"""
    script = _ORPHAN_PRUNE.replace("{root}", root)
    problems = []
    if "{root}" in script:
        problems.append("unexpanded {root} marker remains")
    if "${svc}" not in script:
        problems.append("shell interpolation ${svc} lost")
    # 合法大括号:${...} shell 变量、{{...}} docker inspect Go 模板。
    # 要抓的是残留的 str.format 占位符单大括号({root}/{svc}/...)。
    if re.search(r"(?<![\${])\{(?!\{)", script):
        problems.append("leftover single brace (unexpanded format placeholder)")
    if problems:
        sys.exit(f"orphan-prune script render failed: {'; '.join(problems)}")
    _bash_syntax_check(script)
    return script


def _bash_syntax_check(script: str, bash: str | None = None):
    """bash 可用时对渲染脚本做 bash -n 语法检查(健全性防线)。
    先探活:Windows 上 PATH 里的 bash 可能是 WSL relay(System32\\bash.exe),
    subprocess 起不来——探活失败就跳过,不硬失败;只有真 bash 报了语法
    错误才退出。"""
    if bash is None:
        bash = shutil.which("bash")
    if not bash:
        return
    try:
        probe = subprocess.run([bash, "--version"], capture_output=True, timeout=15)
        runnable = probe.returncode == 0
    except (OSError, subprocess.SubprocessError):
        runnable = False
    if not runnable:
        return
    p = subprocess.run([bash, "-n"], input=script, text=True,
                       capture_output=True, timeout=15)
    if p.returncode != 0:
        sys.exit("orphan-prune script failed bash -n syntax check:\n"
                 + (p.stderr or p.stdout or "unknown error"))


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
                   filter=lambda ti: None if ti.name.endswith(".env") else ti)
        # Web UI source（Docker 内构建）；排除 node_modules 与 dist
        tf.add(ROOT / "web", arcname="web",
               filter=lambda ti: None if "node_modules" in ti.name or "/dist" in ti.name
               or ti.name.endswith(".env") else ti)
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
    env = load_env()
    # TURN vars: from local deploy/.env if set, else defaults (compose mirrors
    # the same defaults, so older remote .env files keep working unchanged).
    turn = {
        "XNC_TURN_URLS": env.get("XNC_TURN_URLS", "turn:control.xnc.app:3478?transport=tcp"),
        "XNC_TURN_USERNAME": env.get("XNC_TURN_USERNAME", "xncdev"),
        "XNC_TURN_PASSWORD": env.get("XNC_TURN_PASSWORD", "xncdev-secret"),
        # TURN 池（境内媒体中转）：逗号分隔 ip[:port]，空 = 未配置（沿用
        # XNC_TURN_URLS）。缺失时追加空键，保证远程 .env 显式可见、后续可改。
        "XNC_TURN_POOL": env.get("XNC_TURN_POOL", ""),
    }
    # XNC_VERSION: server 构建版本（版本单一来源，与 agent bundle 版本一致）。
    # 仅本地 .env 显式设置时同步——未设置即回落 0.0.0-dev，不强加默认值。
    if "XNC_VERSION" in env:
        turn["XNC_VERSION"] = env["XNC_VERSION"]
    code, out = run(client, f"test -s {REMOTE_ROOT}/deploy/.env && echo EXISTS")
    if code == 0:
        # Append any missing TURN keys; never touch existing values.
        sftp = client.open_sftp()
        with sftp.open(f"{REMOTE_ROOT}/deploy/.env") as f:
            cur = f.read().decode()
        missing = {k: v for k, v in turn.items() if f"\n{k}=" not in cur and not cur.startswith(f"{k}=")}
        if missing:
            with sftp.open(f"{REMOTE_ROOT}/deploy/.env", "a") as f:
                f.write("".join(f"{k}={v}\n" for k, v in missing.items()))
            print(f"appended TURN vars to remote .env: {', '.join(missing)}")
        else:
            print("remote deploy/.env exists, keeping it")
        sftp.close()
        return
    admin_email = env.get("SRV_ADMIN_EMAIL") or "admin@xnc.app"
    values = {
        "XNC_DOMAIN": env["SRV_DOMAIN"],
        "POSTGRES_PASSWORD": secrets.token_urlsafe(24),
        "XNC_JWT_SECRET": secrets.token_urlsafe(48),
        "XNC_ADMIN_EMAIL": admin_email,
        "XNC_ADMIN_PASSWORD": secrets.token_urlsafe(16),
        **turn,
    }
    body = "\n".join(f"{k}={v}" for k, v in values.items()) + "\n"
    sftp = client.open_sftp()
    with sftp.open(f"{REMOTE_ROOT}/deploy/.env", "w") as f:
        f.write(body)
    sftp.close()
    sh(f"chmod 600 {REMOTE_ROOT}/deploy/.env && wc -l {REMOTE_ROOT}/deploy/.env")
    _record_local_admin(values["XNC_ADMIN_EMAIL"], values["XNC_ADMIN_PASSWORD"])
    print("remote .env written (600); admin credentials recorded to deploy/.env")


def _record_local_admin(email: str, password: str):
    p = ROOT / "deploy" / ".env"
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


def _compose_up(cmd: str, timeout: int = 900):
    """docker compose up,带一次 Conflict 重试:旧世代孤儿容器
    (2026-08-24 retro P2#10,d00097cd9d8c_* 残留)会让 up 报
    "container name ... is already in use by container <id>" —— 按报错
    精确 rm 该容器后重试一次(比盲删安全)。"""
    code, out = run(client, cmd, timeout=timeout)
    print(f"$ {cmd}\n{out}")
    if code == 0:
        return
    m = re.search(r'is already in use by container "?([0-9a-f]{12,})"?', out)
    if not m:
        sys.exit(f"remote command failed ({code}): {cmd}")
    cid = m.group(1)
    print(f"compose Conflict on container {cid}: removing and retrying once")
    sh(f"docker rm -f {cid}")
    sh(cmd, timeout=timeout)


def cmd_up():
    _write_remote_turn_conf()
    sh(f"cd {REMOTE_ROOT} && " + _orphan_prune_script(REMOTE_ROOT), timeout=120)
    _compose_up(f"cd {REMOTE_ROOT} && docker compose -f deploy/docker-compose.yml up -d --build")
    sh(f"cd {REMOTE_ROOT} && docker compose -f deploy/docker-compose.yml ps --format 'table {{{{.Name}}}}\\t{{{{.Status}}}}'")


def cmd_logs(service: str):
    sh(f"cd {REMOTE_ROOT} && docker compose -f deploy/docker-compose.yml logs --tail=60 {service}".rstrip())


def cmd_verify():
    # 渲染残留检查:远端 turnserver.conf 非注释行不应再有未展开的 ${...
    # (模板变量缺 env/默认值或拼写错误会残留;模板头部注释里的 ${VAR}
    # 说明不参与插值,故跳过 # 行,避免误报)。
    code, resid = run(client,
                      "awk '!/^[[:space:]]*#/ && /\\$\\{/ {print NR \": \" $0}' "
                      f"{REMOTE_ROOT}/deploy/turnserver.conf")
    if resid.strip():
        sys.exit("rendered turnserver.conf still contains unexpanded template "
                 "markers (${...) — missing .env value or bad ${VAR} syntax?:\n"
                 + resid.strip())
    sh(f"cd {REMOTE_ROOT} && docker compose -f deploy/docker-compose.yml ps")
    sh("docker exec $(docker ps -qf name=xnc-server) /xnc-server -healthcheck && echo HEALTHCHECK-OK")
    env = load_env()
    sh(f"sleep 3; curl -sS -m 20 https://{env['SRV_DOMAIN']}/api/health && echo")
    # STUN 自检(Task 6):coturn 监听端口在本机 3478(UDP+TCP)发 STUN
    # binding,期望 0x0101 应答;失败即 verify 失败。
    sh(_remote_py(_STUN_PROBE), timeout=60)


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
