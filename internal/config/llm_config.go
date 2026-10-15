// Package config llm_config.go — LLM 参数显式校验。
//
// 对应 TradingAgents 的 _coerce_max_retries（显式校验参数类型和取值范围）。
//
// 关键设计：
//   - 启动时 fail-fast：配置非法时立即报错，不允许静默降级
//   - 类型安全：bool 不能当 int 用（TradingAgents 踩过这个坑）
//   - 取值边界：temperature 不能 <0 或 >2（OpenAI 官方约束）
package config

import (
	"fmt"
	"strings"
)

// LLMProviderCfg 单一 LLM 平台配置（未来多平台预留）
type LLMProviderCfg struct {
	Name         string  `json:"name"`           // 平台名（deepseek/openai/anthropic/google）
	Model        string  `json:"model"`          // 模型 ID
	APIKey       string  `json:"api_key"`        // API Key（可用 secretbox 加密）
	BaseURL      string  `json:"base_url"`       // API endpoint
	Temperature  float64 `json:"temperature"`    // 0.0~2.0
	MaxTokens    int     `json:"max_tokens"`     // ≤模型上限
	MaxRetries   int     `json:"max_retries"`    // ≥0
	Enabled      bool    `json:"enabled"`        // 是否启用
}

// ValidateLLMConfig 校验 LLM 配置。返回 error 表示配置非法（启动时 fail-fast）。
// 对应 TradingAgents 的 _coerce_max_retries + _get_provider_kwargs 参数校验。
func ValidateLLMConfig(cfg *LLMProviderCfg) error {
	if cfg == nil {
		return fmt.Errorf("LLM config is nil")
	}
	if !cfg.Enabled {
		return nil // 禁用的 provider 不校验
	}

	// 1) Provider name 必须是已知值
	name := strings.ToLower(strings.TrimSpace(cfg.Name))
	switch name {
	case "deepseek", "openai", "anthropic", "google", "":
		// 允许空名（默认 deepseek）
	default:
		return fmt.Errorf("llm.name %q 不是已知平台（deepseek/openai/anthropic/google）", cfg.Name)
	}
	if name == "" {
		name = "deepseek"
	}

	// 2) Model 非空
	if strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("llm.model 不能为空（provider=%s）", name)
	}

	// 3) Temperature ∈ [0.0, 2.0]（OpenAI 官方约束；其他平台兼容）
	if cfg.Temperature < 0 || cfg.Temperature > 2 {
		return fmt.Errorf("llm.temperature %.2f 越界，允许范围 [0.0, 2.0]（provider=%s）",
			cfg.Temperature, name)
	}

	// 4) MaxTokens > 0（OpenAI deepseek 上限 4096~128000，保守取 200000）
	if cfg.MaxTokens <= 0 {
		return fmt.Errorf("llm.max_tokens %d 必须 > 0（provider=%s）", cfg.MaxTokens, name)
	}
	if cfg.MaxTokens > 200000 {
		return fmt.Errorf("llm.max_tokens %d 超过 200000 上限（provider=%s）", cfg.MaxTokens, name)
	}

	// 5) MaxRetries ≥ 0（int 而非 bool——TradingAgents 踩过的坑：bool(true) 被当作 1 但类型不匹配）
	if cfg.MaxRetries < 0 {
		return fmt.Errorf("llm.max_retries %d 必须 ≥ 0（provider=%s）", cfg.MaxRetries, name)
	}
	if cfg.MaxRetries > 10 {
		return fmt.Errorf("llm.max_retries %d 超过 10 上限（provider=%s）——重试太多会掩盖真实故障",
			cfg.MaxRetries, name)
	}

	// 6) API Key 非空（deepseek/openai/anthropic 要求）
	if name != "google" && strings.TrimSpace(cfg.APIKey) == "" {
		return fmt.Errorf("llm.api_key 不能为空（provider=%s）", name)
	}

	return nil
}

// ValidateAllProviders 批量校验所有 provider
func ValidateAllProviders(cfgs []*LLMProviderCfg) error {
	for i, c := range cfgs {
		if err := ValidateLLMConfig(c); err != nil {
			return fmt.Errorf("provider[%d]: %w", i, err)
		}
	}
	return nil
}
