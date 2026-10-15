// Package portfolio trading_memory_log.go — 交易记忆反思闭环。
//
// 对应 TradingAgents 的 agents/utils/memory.py（TradingMemoryLog）。
//
// 核心闭环：
//  1. CIO 每次决策后 → StoreDecision(pending)（无 LLM 调用，纯 IO）
//  2. 每日复盘阶段 → ResolvePending：用 DuckDB 算实际收益 vs 沪深300 alpha
//     + 调 LLM 生成反思 → 原子写入
//  3. 下次 CIO 决策前 → GetPastContext：same-ticker 近 5 条 + cross-ticker 近 3 条
//     拼到 prompt 最前面 → 让决策"吃一堑长一智"
//
// 关键设计：
//   - Append-only JSONL（每行一个 JSON 对象），原子写入避免 crash 损坏
//   - Idempotency guard：同日同标的不重复存
//   - Rotation：超过 max_entries 时删最老已解决条目（pending 永远保留）
//   - 零依赖：只用标准库 encoding/json 和 os，不动 DuckDB/SQLite
package portfolio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// MemoryLogEntry 单条决策记忆
type MemoryLogEntry struct {
	ID        string  `json:"id"`
	TradeDate string  `json:"trade_date"` // YYYY-MM-DD
	Ticker    string  `json:"ticker"`     // 纯数字代码如 002437 或指数代码 sh000300
	Direction string  `json:"direction"`  // buy/sell/hold/scale_in/scale_out
	Position  float64 `json:"position"`   // 建议目标仓位比例 0.0~1.0
	Reason    string  `json:"reason"`     // LLM 决策理由（≤200 字）

	// 以下字段在 Resolve 阶段填充（pending → resolved）
	Pending     bool    `json:"pending"`
	HoldingDays int     `json:"holding_days,omitempty"`
	RawReturn   float64 `json:"raw_return,omitempty"`   // 实际持有期收益
	AlphaReturn float64 `json:"alpha_return,omitempty"` // 相对沪深300 alpha = raw - benchmark
	Reflection  string  `json:"reflection,omitempty"`   // LLM 反思（"这次判断对/错在哪..."）

	CreatedAt  string `json:"created_at"`
	ResolvedAt string `json:"resolved_at,omitempty"`
}

// TradingMemoryLog 交易记忆日志
type TradingMemoryLog struct {
	mu         sync.Mutex
	logPath    string
	maxEntries int
}

// NewTradingMemoryLog 创建交易记忆日志。logPath 是 JSONL 文件路径。
// maxEntries=0 表示不做 rotation。
func NewTradingMemoryLog(logPath string, maxEntries int) (*TradingMemoryLog, error) {
	if logPath == "" {
		return nil, fmt.Errorf("logPath is empty")
	}
	abs, err := filepath.Abs(logPath)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	return &TradingMemoryLog{
		logPath:    abs,
		maxEntries: maxEntries,
	}, nil
}

// StoreDecision 存一条 pending 决策。幂等：同日同标的同方向已存在则跳过。
func (l *TradingMemoryLog) StoreDecision(e MemoryLogEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e.TradeDate == "" {
		e.TradeDate = time.Now().Format("2006-01-02")
	}
	if e.CreatedAt == "" {
		e.CreatedAt = time.Now().Format(time.RFC3339)
	}
	if e.ID == "" {
		e.ID = fmt.Sprintf("%s|%s|%s", e.TradeDate, e.Ticker, e.Direction)
	}
	e.Pending = true

	// Idempotency guard
	existing, _ := l.loadLocked()
	for _, x := range existing {
		if x.TradeDate == e.TradeDate && x.Ticker == e.Ticker && x.Direction == e.Direction && x.Pending {
			return nil // 已存在，跳过
		}
	}

	// 截断 reason 到 200 字
	if len(e.Reason) > 200 {
		e.Reason = e.Reason[:200] + "…"
	}

	// Append 一行
	f, err := os.OpenFile(l.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// LoadAll 加载全部条目（按日期+ID 升序）
func (l *TradingMemoryLog) LoadAll() ([]MemoryLogEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadLocked()
}

func (l *TradingMemoryLog) loadLocked() ([]MemoryLogEntry, error) {
	data, err := os.ReadFile(l.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []MemoryLogEntry
	for _, line := range splitLines(data) {
		if line == "" {
			continue
		}
		var e MemoryLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 跳过坏行
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].TradeDate != entries[j].TradeDate {
			return entries[i].TradeDate < entries[j].TradeDate
		}
		return entries[i].ID < entries[j].ID
	})
	return entries, nil
}

// UpdateResolved 把 pending 条目标记为已解决，填收益+反思。原子写入。
// id = TradeDate|Ticker|Direction
func (l *TradingMemoryLog) UpdateResolved(id string, rawReturn, alphaReturn float64, holdingDays int, reflection string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := l.loadLocked()
	if err != nil {
		return err
	}

	updated := false
	for i, e := range entries {
		if e.ID == id && e.Pending {
			entries[i].Pending = false
			entries[i].RawReturn = rawReturn
			entries[i].AlphaReturn = alphaReturn
			entries[i].HoldingDays = holdingDays
			entries[i].Reflection = reflection
			entries[i].ResolvedAt = time.Now().Format(time.RFC3339)
			updated = true
			break
		}
	}
	if !updated {
		return fmt.Errorf("entry %q not found or already resolved", id)
	}

	// 原子写：tmp + rename
	tmpPath := l.logPath + ".tmp"
	if err := writeJSONL(tmpPath, entries); err != nil {
		return err
	}
	return os.Rename(tmpPath, l.logPath)
}

// GetPastContext 取决策上下文注入。
// same-ticker: 近 nSame 条已解决；cross-ticker: 近 nCross 条已解决（其他标的）。
func (l *TradingMemoryLog) GetPastContext(ticker string, nSame, nCross int) string {
	entries, err := l.LoadAll()
	if err != nil || len(entries) == 0 {
		return ""
	}
	var same, cross []MemoryLogEntry
	for i := len(entries) - 1; i >= 0; i-- { // 从新到旧
		e := entries[i]
		if e.Pending {
			continue
		}
		if e.Ticker == ticker && len(same) < nSame {
			same = append(same, e)
		} else if e.Ticker != ticker && len(cross) < nCross {
			cross = append(cross, e)
		}
		if len(same) >= nSame && len(cross) >= nCross {
			break
		}
	}
	var sb string
	for _, e := range same {
		sb += fmt.Sprintf("[%s | %s | %s | raw=%.1f%% alpha=%.1f%% | %d天] DECISION: %s\n  REFLECTION: %s\n\n",
			e.TradeDate, e.Ticker, e.Direction, e.RawReturn*100, e.AlphaReturn*100,
			e.HoldingDays, truncateStr(e.Reason, 100), truncateStr(e.Reflection, 150))
	}
	for _, e := range cross {
		sb += fmt.Sprintf("[%s | %s | %s | raw=%.1f%% alpha=%.1f%%]\n  REFLECTION: %s\n\n",
			e.TradeDate, e.Ticker, e.Direction, e.RawReturn*100, e.AlphaReturn*100,
			truncateStr(e.Reflection, 150))
	}
	return sb
}

// === helpers ===

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var lines []string
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, string(data[start:i]))
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	return lines
}

func writeJSONL(path string, entries []MemoryLogEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		data = append(data, '\n')
		if _, err := f.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func truncateStr(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
