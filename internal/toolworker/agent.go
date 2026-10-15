// Package toolworker 宿主侧封装 agentbridge 的拉模式 RPC 总线，并承载确定性的
// 工具/持久化/上下文兑现 handler。
//
// 重构说明：原 agent.dll C ABI 桥接层已合并进 internal/agentbridge，
// 本包的 syscall.LoadDLL 层已消除。Agent 现在直接持有 bridge 会话，
// Init/RunDaily/Poll/Respond/CloseSession 方法签名不变（入参/出参都是
// JSON string），runner.go 无需任何修改。
package toolworker

import (
	"fmt"

	"github.com/quantpilot/quantpilot/internal/agentbridge"
)

// Agent 封装对 agentbridge 的调用。Load 返回的 Agent 不再需要 Close()，
// bridge 会话在 CloseSession(handle) 时自动清理。
type Agent struct {
	Debug bool
}

// Load 返回一个可用的 Agent（不再加载 DLL）。path 参数保留兼容旧调用方。
func Load(_ string) (*Agent, error) {
	return &Agent{}, nil
}

// Close 退出决策脑使用。bridge 会话由 CloseSession 按需清理，无需全局释放。
func (a *Agent) Close() {}

// call 统一封装 bridge 函数调用（bridge 函数直接返回 string，无需 C 内存释放）。
func (a *Agent) call(fn func() string) (string, error) {
	return fn(), nil
}

func (a *Agent) Version() (string, error) {
	return agentbridge.AgentVersion(), nil
}

func (a *Agent) Ping(msg string) (string, error) {
	return agentbridge.AgentPing(msg), nil
}

func (a *Agent) Abi() (string, error) {
	return agentbridge.AgentAbi(), nil
}

func (a *Agent) Init(configJSON string) (string, error) {
	return agentbridge.AgentInit(configJSON), nil
}

func (a *Agent) RunDaily(handle int64, runJSON string) (string, error) {
	return agentbridge.AgentRunDailyCycle(handle, runJSON), nil
}

func (a *Agent) Poll(handle int64) (string, error) {
	return agentbridge.AgentPoll(handle), nil
}

func (a *Agent) Respond(handle int64, id, resultJSON string) (string, error) {
	return agentbridge.AgentRespond(handle, id, resultJSON), nil
}

func (a *Agent) CloseSession(handle int64) (string, error) {
	return agentbridge.AgentClose(handle), nil
}

// normalizeErr 统一把非 ok 的响应 JSON 转为 error。
func normalizeErr(raw string, err error) error {
	if err != nil {
		return err
	}
	// 简单检查：包含 `"ok":false` 视为错误
	if len(raw) >= 8 && raw[:8] == "{\"ok\":false" {
		return fmt.Errorf("bridge 返回错误: %s", raw)
	}
	return nil
}
