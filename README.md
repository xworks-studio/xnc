# XNC v2

Windows 节点统一运维入口，出站 443 反向连接（agent 主动连 server，不开入站端口）。

设计文档：[spec.md](spec.md)；计划与规格：[docs/](docs/)。

## 负载 smoke

```bash
make dev-up && make load N=1000
# 观察：docker stats 中 xnc-server CPU 应接近空闲（1000 心跳 ≈ 33 msg/s）
# 验证 1000 节点在线：xnc node list | grep -c online
```

（`xnc` 路径同 e2e；Windows 下 bin/xnc.exe。）

无 make 的环境（如本 dev 机 Git Bash）等价命令：

```bash
cd deploy && docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build && cd ..
(cd cli && go build -o ../bin/xnc.exe .) && (cd mockagent && go build -o ../bin/mockagent.exe .)
export XNC_SERVER=http://127.0.0.1:8080 XNC_TOKEN=$(printf 'change-me' | bin/xnc.exe login --email admin@example.com --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
bin/mockagent.exe --server http://127.0.0.1:8080 --token "$(bin/xnc.exe token create default --max-uses 1000 --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')" --count 1000
```

注：`node list --json` 输出为单行，计数用 `grep -o '"status":"online"' | wc -l`（`grep -c` 数的是行数）；表格模式（不带 --json）逐行输出，`grep -c online` 可用。Ctrl+C 结束后 `cd deploy && docker compose -f docker-compose.yml -f docker-compose.dev.yml down -v` 清理。
