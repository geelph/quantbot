// Package cio checkpoint.go — CIO 决策链断点恢复。
//
// 对应 TradingAgents 的 checkpointer.py（LangGraph SqliteSaver + thread_id）。
//
// 设计：
//   - 每节点 checkpoint（genDecisionProposal / debate / portfolio manager 等）
//   - checkpoint key = portfolioID + trade_date + step_name
//   - 崩溃后重启，CIO 检测到 checkpoint 存在 → 从断点续跑而非重新开始
//   - 用 JSON 序列化节点输出（纯 Go，无依赖）
//   - 完成整个决策链后 clear（清理旧 checkpoint，避免下次误 resume）
package cio

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/quantpilot/quantpilot/internal/data"
)

// CheckpointNode 决策链节点名（和 makeDecision 里的节点顺序一致）
const (
	CheckpointPreResearch   = "pre_research"   // Phase A 前置调研
	CheckpointProposal      = "proposal"        // Phase B 严格 JSON 决策
	CheckpointDebate        = "debate"          // 三立场辩论
	CheckpointEnsemble      = "ensemble"        // 集成投票
	CheckpointPortfolioMgr  = "portfolio_mgr"   // Portfolio Manager 最终决策
	CheckpointDone          = "done"            // 整条链完成（清理标记）
)

// CIODecisionCheckpoint CIO 决策链 checkpoint
type CIODecisionCheckpoint struct {
	PortfolioID  string    `json:"portfolio_id"`
	TradeDate    string    `json:"trade_date"`
	Step         string    `json:"step"`          // 当前已完成的节点
	Payload      string    `json:"payload"`       // 节点输出的 JSON 序列化（nil 也能存）
	CreatedAt    time.Time `json:"created_at"`
}

// CIODecisionCheckpointer CIO 决策链 checkpoint 管理器
type CIODecisionCheckpointer struct {
	sqlite *data.SQLiteManager
}

// NewCIODecisionCheckpointer 创建 checkpoint 管理器
func NewCIODecisionCheckpointer(db *data.SQLiteManager) *CIODecisionCheckpointer {
	return &CIODecisionCheckpointer{sqlite: db}
}

// tableName checkpoint 表名（和业务表隔离）
const cioCheckpointTable = "cio_decision_checkpoints"

// SaveCheckpoint 存一个节点 checkpoint
func (cp *CIODecisionCheckpointer) SaveCheckpoint(ctx context.Context, portfolioID, tradeDate, step string, payload interface{}) error {
	if cp == nil || cp.sqlite == nil {
		return nil // 无 db 时静默跳过
	}
	var payloadStr string
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal checkpoint payload: %w", err)
		}
		payloadStr = string(b)
	}
	ckp := CIODecisionCheckpoint{
		PortfolioID: portfolioID,
		TradeDate:   tradeDate,
		Step:        step,
		Payload:     payloadStr,
		CreatedAt:   time.Now(),
	}
	db := cp.sqlite.GetDB()
	if db == nil {
		return nil
	}
	// 同 portfolio+date 可能有多次 step → 覆盖更新（upsert）
	return db.WithContext(ctx).Where(
		"portfolio_id = ? AND trade_date = ? AND step = ?",
		portfolioID, tradeDate, step,
	).Assign(ckp).FirstOrCreate(&ckp).Error
}

// LoadLatestStep 加载某 portfolio+date 已完成的最新 step
// 返回 step 名称 + 是否已到达 done。无 checkpoint 时 step=""
func (cp *CIODecisionCheckpointer) LoadLatestStep(ctx context.Context, portfolioID, tradeDate string) (step string, reachedDone bool, err error) {
	if cp == nil || cp.sqlite == nil {
		return "", false, nil
	}
	db := cp.sqlite.GetDB()
	if db == nil {
		return "", false, nil
	}
	var ckps []CIODecisionCheckpoint
	if err := db.WithContext(ctx).Where(
		"portfolio_id = ? AND trade_date = ?", portfolioID, tradeDate,
	).Order("created_at desc").Find(&ckps).Error; err != nil {
		return "", false, err
	}
	if len(ckps) == 0 {
		return "", false, nil
	}
	step = ckps[0].Step
	reachedDone = step == CheckpointDone
	return step, reachedDone, nil
}

// LoadCheckpointPayload 加载指定 step 的 payload（反序列化）
// out 必须是 *struct（json.Unmarshal 目标）
func (cp *CIODecisionCheckpointer) LoadCheckpointPayload(ctx context.Context, portfolioID, tradeDate, step string, out interface{}) (bool, error) {
	if cp == nil || cp.sqlite == nil {
		return false, nil
	}
	db := cp.sqlite.GetDB()
	if db == nil {
		return false, nil
	}
	var ckp CIODecisionCheckpoint
	if err := db.WithContext(ctx).Where(
		"portfolio_id = ? AND trade_date = ? AND step = ?",
		portfolioID, tradeDate, step,
	).First(&ckp).Error; err != nil {
		return false, nil // 不存在 → 不是错误
	}
	if ckp.Payload == "" {
		return false, nil
	}
	if out == nil {
		return true, nil
	}
	if err := json.Unmarshal([]byte(ckp.Payload), out); err != nil {
		return true, fmt.Errorf("unmarshal checkpoint %s/%s/%s: %w", portfolioID, tradeDate, step, err)
	}
	return true, nil
}

// ClearCheckpoints 完成整条决策链后清理（避免下次误 resume）
func (cp *CIODecisionCheckpointer) ClearCheckpoints(ctx context.Context, portfolioID, tradeDate string) error {
	if cp == nil || cp.sqlite == nil {
		return nil
	}
	db := cp.sqlite.GetDB()
	if db == nil {
		return nil
	}
	return db.WithContext(ctx).Where(
		"portfolio_id = ? AND trade_date = ?", portfolioID, tradeDate,
	).Delete(&CIODecisionCheckpoint{}).Error
}

// DecisionStepOrder 决策链节点顺序（用于 resume：跳到下一个未完成的节点）
var DecisionStepOrder = []string{
	CheckpointPreResearch,
	CheckpointProposal,
	CheckpointDebate,
	CheckpointEnsemble,
	CheckpointPortfolioMgr,
	CheckpointDone,
}

// NextStep 从已完成的 step 名跳到下一个待执行节点。
// 传入 step=""（无 checkpoint）时返回第一个节点。
// 传入 step=CheckpointDone 时返回 ""（已完成，不应再跑）。
func NextStep(completedStep string) string {
	for i, s := range DecisionStepOrder {
		if s == completedStep {
			if i+1 < len(DecisionStepOrder) {
				return DecisionStepOrder[i+1]
			}
			return "" // 已完成
		}
	}
	// 没找到 → 从头开始
	return DecisionStepOrder[0]
}
