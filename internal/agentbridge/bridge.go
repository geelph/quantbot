// Package agentbridge 把原 agent.dll 的 C ABI 桥接层改成进程内 Go 包。
//
// 重构目标：消除 syscall.LoadDLL 和 c-shared 边界，让决策脑与宿主在同一个
// Go runtime 里运行——不再需要拉模式 RPC 总线的跨边界 JSON 编解码开销。
//
// 外部接口（Init / RunDailyCycle / Poll / Respond / Close）与原 DLL 的
// //export 函数签名保持一致：入参/出参都是 string（JSON 字符串），这样
// toolworker/runner.go 无需任何修改即可直调本包。
package agentbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quantpilot/quantpilot/internal/brain/agents"
	"github.com/quantpilot/quantpilot/internal/brain/llm"
	"github.com/quantpilot/quantpilot/internal/brain/port"
	"github.com/quantpilot/quantpilot/internal/brain/toolkit"
)

// API 版本与 ABI 版本。
const (
	apiVersion = "1.0.0"
	abiVersion = "3"
)

// jsonResponse 统一封装响应 JSON（与原 DLL 的 jsonResponse 格式完全一致）。
func jsonResponse(ok bool, data interface{}, errMsg string) string {
	b, _ := json.Marshal(map[string]interface{}{
		"ok":    ok,
		"error": errMsg,
		"data":  data,
	})
	return string(b)
}

// AgentVersion 返回决策脑版本与 ABI 版本。
func AgentVersion() string {
	return jsonResponse(true, map[string]string{
		"version": apiVersion,
		"abi":     abiVersion,
	}, "")
}

// AgentPing 联通性自检：回显入参原样返回。
func AgentPing(msg string) string {
	var echo interface{}
	_ = json.Unmarshal([]byte(msg), &echo)
	return jsonResponse(true, map[string]interface{}{
		"echo":   msg,
		"parsed": echo != nil,
	}, "")
}

// AgentAbi 返回本桥的完整 ABI 清单。
func AgentAbi() string {
	ops := []map[string]string{
		{"name": "agent_version", "method": "AgentVersion", "response": "{ok,error,data:{version,abi}}"},
		{"name": "agent_ping", "method": "AgentPing", "response": "{ok,error,data:{echo,parsed}}"},
		{"name": "agent_init", "method": "AgentInit", "request": "{config:{market?,date?,timeout_minutes?,llm:{provider?,api_key?,base_url?,model?}},catalog:[{role,tools:[{name,description,parameters}]}]}", "response": "{ok,error,data:{handle,config,wired,abi}}"},
		{"name": "agent_run_daily", "method": "AgentRunDailyCycle", "request": "{date?}", "response": "{ok,error,data:{handle,date,status}}"},
		{"name": "agent_poll", "method": "AgentPoll", "request": "{handle}", "response": "{ok,error,data:{status,request?,result?,error?}}"},
		{"name": "agent_respond", "method": "AgentRespond", "request": "{handle,id,result}", "response": "{ok,error,data:{handle,id,delivered}}"},
		{"name": "agent_close", "method": "AgentClose", "request": "{handle}", "response": "{ok,error,data:{handle,closed}}"},
	}
	return jsonResponse(true, map[string]interface{}{
		"abi":         abiVersion,
		"api_version": apiVersion,
		"brain":       buildBrainManifest(),
		"operations":  ops,
	}, "")
}

// -------- 决策脑句柄注册表 --------

// abiConfig AgentInit 请求的会话配置。
type abiConfig struct {
	Market         string `json:"market,omitempty"`
	Date           string `json:"date,omitempty"`
	TimeoutMinutes int    `json:"timeout_minutes,omitempty"`

	Provider string `json:"provider,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Model    string `json:"model,omitempty"`
}

// busCatalog 角色目录元数据。
type busCatalog struct {
	Role  string           `json:"role"`
	Tools []busCatalogTool `json:"tools"`
}

type busCatalogTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// buildBrainManifest 汇总编译进本桥的决策脑子包。
func buildBrainManifest() []string {
	return []string{"agents", "llm", "port", "toolkit"}
}

// runNotWired 可运行语义角色标签。
const runNotWired = "not_wired"

var registryMu sync.Mutex
var registryNext int64
var registry = make(map[int64]*brainSession)

func registryCreate(cfg abiConfig) int64 {
	registryMu.Lock()
	defer registryMu.Unlock()
	registryNext++
	id := registryNext
	registry[id] = &brainSession{
		ID:        id,
		Config:    cfg,
		ToolWired: false,
		waiters:   make(map[string]chan busReply),
	}
	return id
}

func registryGet(id int64) (*brainSession, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	s, ok := registry[id]
	return s, ok
}

func registryClose(id int64) bool {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[id]; !ok {
		return false
	}
	delete(registry, id)
	return true
}

// -------- 拉模式 RPC 总线（进程内版本，不再跨运行时）--------

type busRequest struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

type busReply struct {
	Result json.RawMessage
	Err    string
}

const (
	kindTool    = "tool"
	kindStore   = "store"
	kindContext = "context"
)

type busError struct{ msg string }

func (e *busError) Error() string { return e.msg }

// brainSession 一次 AgentInit 对应的会话元数据。
type brainSession struct {
	ID        int64
	Config    abiConfig
	ToolWired bool

	busMu        sync.Mutex
	pendingReq   []busRequest
	waiters      map[string]chan busReply
	requestIDSeq int64

	orch          *agents.Orchestrator
	remotePersist port.Persistence
	remoteLLM     llm.Client
	ctxProvider   port.ContextProvider
	cancel        context.CancelFunc

	running bool
	done    bool
	result  *agents.OrchestratorResult
	err     error
}

func (s *brainSession) sendRequest(ctx context.Context, kind string, payload interface{}) (json.RawMessage, error) {
	pb, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	id := atomic.AddInt64(&s.requestIDSeq, 1)
	idStr := strconv.FormatInt(id, 10)
	ch := make(chan busReply, 1)
	req := busRequest{ID: idStr, Kind: kind, Payload: pb}

	s.busMu.Lock()
	s.pendingReq = append(s.pendingReq, req)
	s.waiters[idStr] = ch
	s.busMu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case rep := <-ch:
		if rep.Err != "" {
			return nil, &busError{rep.Err}
		}
		return rep.Result, nil
	}
}

type pollResult struct {
	Status  string          `json:"status"`
	Request *busRequest     `json:"request,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func (s *brainSession) poll() pollResult {
	s.busMu.Lock()
	defer s.busMu.Unlock()

	if s.done {
		if s.err != nil {
			return pollResult{Status: "error", Error: s.err.Error()}
		}
		if s.result != nil {
			rb, _ := json.Marshal(s.result)
			return pollResult{Status: "done", Result: rb}
		}
		return pollResult{Status: "error", Error: "分析周期未产生结果"}
	}

	if len(s.pendingReq) > 0 {
		req := s.pendingReq[0]
		s.pendingReq = s.pendingReq[1:]
		return pollResult{Status: "awaiting_request", Request: &req}
	}

	return pollResult{Status: "running"}
}

func (s *brainSession) respondRequest(id, resultJSON string) bool {
	s.busMu.Lock()
	defer s.busMu.Unlock()
	ch, ok := s.waiters[id]
	if !ok {
		return false
	}
	delete(s.waiters, id)
	ch <- busReply{Result: json.RawMessage(resultJSON)}
	return true
}

func (s *brainSession) isRunningUnsafe() bool {
	s.busMu.Lock()
	defer s.busMu.Unlock()
	return s.running
}

// -------- 远程包装：实现 port / toolkit 接口 --------

type remotePersistence struct{ s *brainSession }

func newRemotePersistence(s *brainSession) *remotePersistence { return &remotePersistence{s: s} }

func (r *remotePersistence) ClearTaskLogs(taskDate string) error {
	_, err := r.s.sendRequest(context.Background(), kindStore, struct {
		Op   string `json:"op"`
		Date string `json:"date"`
	}{Op: "clear_task_logs", Date: taskDate})
	return err
}

func (r *remotePersistence) LogTaskStart(taskDate, taskPhase, agentRole, taskName string, taskOrder int) *port.TaskLog {
	raw, err := r.s.sendRequest(context.Background(), kindStore, struct {
		Op        string `json:"op"`
		TaskDate  string `json:"task_date"`
		TaskPhase string `json:"task_phase"`
		AgentRole string `json:"agent_role"`
		TaskName  string `json:"task_name"`
		TaskOrder int    `json:"task_order"`
	}{Op: "log_task_start", TaskDate: taskDate, TaskPhase: taskPhase, AgentRole: agentRole, TaskName: taskName, TaskOrder: taskOrder})
	if err != nil {
		return nil
	}
	var resp struct {
		TaskID uint `json:"task_id"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	return &port.TaskLog{ID: resp.TaskID}
}

func (r *remotePersistence) LogTaskComplete(taskID uint, deliverableType, deliverableName string, deliverableData interface{}, summary string) {
	_, _ = r.s.sendRequest(context.Background(), kindStore, struct {
		Op              string      `json:"op"`
		TaskID          uint        `json:"task_id"`
		DeliverableType string      `json:"deliverable_type"`
		DeliverableName string      `json:"deliverable_name"`
		DeliverableData interface{} `json:"deliverable_data"`
		Summary         string      `json:"summary"`
	}{Op: "log_task_complete", TaskID: taskID, DeliverableType: deliverableType, DeliverableName: deliverableName, DeliverableData: deliverableData, Summary: summary})
}

func (r *remotePersistence) LogTaskFailed(taskID uint, errMsg string) {
	_, _ = r.s.sendRequest(context.Background(), kindStore, struct {
		Op     string `json:"op"`
		TaskID uint   `json:"task_id"`
		ErrMsg string `json:"err_msg"`
	}{Op: "log_task_failed", TaskID: taskID, ErrMsg: errMsg})
}

func (r *remotePersistence) WriteAudit(eventID, eventType, userID, userName, action, targetType, targetID, result, detailsJSON string, ts time.Time) error {
	_, err := r.s.sendRequest(context.Background(), kindStore, struct {
		Op          string `json:"op"`
		EventID     string `json:"event_id"`
		EventType   string `json:"event_type"`
		UserID      string `json:"user_id"`
		UserName    string `json:"user_name"`
		Action      string `json:"action"`
		TargetType  string `json:"target_type"`
		TargetID    string `json:"target_id"`
		Result      string `json:"result"`
		DetailsJSON string `json:"details_json"`
		TS          string `json:"ts"`
	}{Op: "write_audit", EventID: eventID, EventType: eventType, UserID: userID, UserName: userName,
		Action: action, TargetType: targetType, TargetID: targetID, Result: result, DetailsJSON: detailsJSON,
		TS: ts.Format(time.RFC3339)})
	return err
}

func (r *remotePersistence) SaveCIODecision(decisionID, portfolioID, decision, reason, riskApproval, policyStatus, marketState string, marketConfidence float64, ts time.Time) error {
	_, err := r.s.sendRequest(context.Background(), kindStore, struct {
		Op               string  `json:"op"`
		DecisionID       string  `json:"decision_id"`
		PortfolioID      string  `json:"portfolio_id"`
		Decision         string  `json:"decision"`
		Reason           string  `json:"reason"`
		RiskApproval     string  `json:"risk_approval"`
		PolicyStatus     string  `json:"policy_status"`
		MarketState      string  `json:"market_state"`
		MarketConfidence float64 `json:"market_confidence"`
		TS               string  `json:"ts"`
	}{Op: "save_cio_decision", DecisionID: decisionID, PortfolioID: portfolioID, Decision: decision, Reason: reason,
		RiskApproval: riskApproval, PolicyStatus: policyStatus, MarketState: marketState,
		MarketConfidence: marketConfidence, TS: ts.Format(time.RFC3339)})
	return err
}

// remoteTool 实现 toolkit.ToolExecutor。
type remoteTool struct {
	s           *brainSession
	name        string
	description string
	parameters  interface{}
}

func newRemoteTool(s *brainSession, name, description string, parameters json.RawMessage) *remoteTool {
	var p interface{}
	if len(parameters) > 0 {
		if err := json.Unmarshal(parameters, &p); err != nil {
			p = parameters
		}
	}
	return &remoteTool{s: s, name: name, description: description, parameters: p}
}

func (t *remoteTool) Name() string                                   { return t.name }
func (t *remoteTool) Description() string                            { return t.description }
func (t *remoteTool) GetDefinition() toolkit.ToolDefinition {
	return toolkit.ToolDefinition{
		Type: "function",
		Function: toolkit.ToolFunction{
			Name:        t.name,
			Description: t.description,
			Parameters:  t.parameters,
		},
	}
}

func (t *remoteTool) Execute(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	raw, err := t.s.sendRequest(ctx, kindTool, struct {
		Name string                 `json:"name"`
		Args map[string]interface{} `json:"args"`
	}{Name: t.name, Args: args})
	if err != nil {
		return nil, err
	}
	var result interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("无法解码宿主工具结果: %w", err)
	}
	return result, nil
}

// -------- 团队构造 --------

var roleMap = map[string]agents.AgentRole{
	"cio":     agents.RoleCIO,
	"planner": agents.RolePlanner,
	"quant":   agents.RoleQuant,
	"risk":    agents.RoleRisk,
	"trader":  agents.RoleTrader,
}

func (s *brainSession) setupTeam(catalog []busCatalog) {
	s.remotePersist = newRemotePersistence(s)
	s.remoteLLM = llm.NewClient(s.Config.Provider, s.Config.APIKey, s.Config.BaseURL, s.Config.Model)
	ctxProvider := func(ctx context.Context, role string, date string) string {
		if ctx == nil {
			ctx = context.Background()
		}
		raw, err := s.sendRequest(ctx, kindContext, struct {
			Role string `json:"role"`
			Date string `json:"date"`
		}{Role: role, Date: date})
		if err != nil {
			return ""
		}
		var result string
		_ = json.Unmarshal(raw, &result)
		return result
	}
	s.ctxProvider = ctxProvider

	orch := agents.NewOrchestrator(s.remotePersist, s.remoteLLM)
	orch.ContextProvider = ctxProvider
	s.orch = orch

	for _, cat := range catalog {
		role := roleMap[cat.Role]
		if role == "" {
			continue
		}

		var remoteTools []port.ToolExecutor
		for _, c := range cat.Tools {
			params := json.RawMessage(nil)
			if c.Parameters != nil {
				params = c.Parameters
			}
			remoteTools = append(remoteTools, newRemoteTool(s, c.Name, c.Description, params))
		}

		baseID := string(role)
		baseAgent := agents.NewBaseAgentWithRole(baseID, role, "")
		baseAgent.SetLLMClient(s.remoteLLM)
		baseAgent.SetMode(agents.ModeStandard)
		for _, t := range remoteTools {
			baseAgent.RegisterTool(t)
		}
		orch.RegisterBaseAgent(baseAgent)

		agent := agents.NewAgent(baseID, role, s.remotePersist, s.remoteLLM, nil)
		for _, t := range remoteTools {
			agent.RegisterTool(t)
		}
		orch.RegisterAgent(agent)
	}

	s.ToolWired = true
}

// -------- AgentInit / AgentRunDailyCycle / AgentPoll / AgentRespond / AgentClose --------

type initRequest struct {
	Config  abiConfig    `json:"config"`
	Catalog []busCatalog `json:"catalog"`
}

// AgentInit 建立一次决策脑会话并装配决策脑团队，返回 JSON（含 handle）。
func AgentInit(configJSON string) string {
	var req initRequest
	if configJSON != "" {
		if err := json.Unmarshal([]byte(configJSON), &req); err != nil {
			return jsonResponse(false, map[string]interface{}{"abi": abiVersion}, "请求体不是合法 JSON: "+err.Error())
		}
	}
	// 兜底：宿主直接传 abiConfig 而非 initRequest 时，将其视为 config。
	if len(req.Catalog) == 0 && !isWrappedConfig(configJSON) {
		if cfg, err := decodeConfig(configJSON); err == nil {
			req.Config = cfg
		}
	}
	if req.Config.Date == "" {
		req.Config.Date = time.Now().Format("2006-01-02")
	}
	handle := registryCreate(req.Config)
	session, ok := registryGet(handle)
	if !ok {
		return jsonResponse(false, map[string]interface{}{"abi": abiVersion}, "会话创建失败")
	}
	session.setupTeam(req.Catalog)
	return jsonResponse(true, map[string]interface{}{
		"handle": handle,
		"config": session.Config,
		"wired":  session.ToolWired,
		"abi":    abiVersion,
	}, "")
}

func isWrappedConfig(raw string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return false
	}
	_, hasConfig := m["config"]
	_, hasCatalog := m["catalog"]
	return hasConfig || hasCatalog
}

func decodeConfig(raw string) (abiConfig, error) {
	var cfg abiConfig
	if raw == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("config 不是合法 JSON: %v", err)
	}
	return cfg, nil
}

// AgentRunDailyCycle 启动后台 goroutine 驱动 Orchestrator.RunDailyCycle，立即返回 started。
func AgentRunDailyCycle(handleID int64, runJSON string) string {
	session, ok := registryGet(handleID)
	if !ok {
		return jsonResponse(false, map[string]interface{}{}, "句柄无效或已关闭")
	}
	var req struct {
		Date string `json:"date,omitempty"`
	}
	_ = json.Unmarshal([]byte(runJSON), &req)
	date := req.Date
	if date == "" {
		date = session.Config.Date
	}
	if !session.ToolWired {
		return jsonResponse(true, map[string]interface{}{
			"handle": handleID, "date": date, "status": runNotWired,
		}, "")
	}
	if session.orch == nil {
		return jsonResponse(false, map[string]interface{}{"handle": handleID, "date": date},
			"决策脑团队未装配（AgentInit 失败）")
	}

	session.busMu.Lock()
	if session.running {
		session.busMu.Unlock()
		return jsonResponse(true, map[string]interface{}{
			"handle": handleID, "date": date, "status": "already_running",
		}, "")
	}
	session.running = true
	session.done = false
	session.result = nil
	session.err = nil
	orch := session.orch
	ctx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	session.busMu.Unlock()

	go func() {
		defer cancel()
		result, err := orch.RunDailyCycle(ctx)
		session.busMu.Lock()
		session.result = result
		session.err = err
		session.done = true
		session.running = false
		session.busMu.Unlock()
	}()

	return jsonResponse(true, map[string]interface{}{
		"handle": handleID,
		"date":   date,
		"status": "started",
	}, "")
}

// AgentPoll 拉取一次会话状态。
func AgentPoll(handleID int64) string {
	session, ok := registryGet(handleID)
	if !ok {
		return jsonResponse(false, map[string]interface{}{}, "句柄无效或已关闭")
	}
	res := session.poll()
	var data interface{}
	switch res.Status {
	case "awaiting_request":
		data = map[string]interface{}{
			"status":  "awaiting_request",
			"running": session.isRunningUnsafe(),
			"request": res.Request,
		}
	case "done":
		data = map[string]interface{}{
			"status":  "done",
			"running": session.isRunningUnsafe(),
			"result":  json.RawMessage(res.Result),
		}
	case "error":
		data = map[string]interface{}{
			"status":  "error",
			"running": session.isRunningUnsafe(),
			"error":   res.Error,
		}
	default:
		data = map[string]interface{}{
			"status":  "running",
			"running": session.isRunningUnsafe(),
		}
	}
	return jsonResponse(true, data, "")
}

// AgentRespond 把宿主兑现的结果推回被阻塞的脑 goroutine。
func AgentRespond(handleID int64, id, resultJSON string) string {
	session, ok := registryGet(handleID)
	if !ok {
		return jsonResponse(false, map[string]interface{}{}, "句柄无效或已关闭")
	}
	delivered := session.respondRequest(id, resultJSON)
	return jsonResponse(true, map[string]interface{}{
		"handle":    handleID,
		"id":        id,
		"delivered": delivered,
	}, "")
}

// AgentClose 取消并释放一次会话句柄。
func AgentClose(handleID int64) string {
	session, ok := registryGet(handleID)
	if !ok {
		return jsonResponse(false, map[string]interface{}{"handle": handleID}, "句柄无效或已关闭")
	}
	session.busMu.Lock()
	if session.cancel != nil {
		session.cancel()
	}
	session.busMu.Unlock()
	if !registryClose(handleID) {
		return jsonResponse(false, map[string]interface{}{"handle": handleID}, "句柄无效或已关闭")
	}
	return jsonResponse(true, map[string]interface{}{"handle": handleID, "closed": true}, "")
}
