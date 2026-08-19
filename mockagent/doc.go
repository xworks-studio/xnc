// Command mockagent 是可脚本化的 agent 克隆：复用真实 agent 核心
// （identity/enroll/connect/machineinfo），为 E2E 与负载冒烟提供可控的
// 多节点模拟。默认把 slog 输出为 stdout JSON，便于测试捕获。
package main
