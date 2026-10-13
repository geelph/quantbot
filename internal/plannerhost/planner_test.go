package planner

import (
	"testing"

	"github.com/quantpilot/quantpilot/internal/port"
)

// TestBearishUsesMarketState 校验弱市判断优先使用选股市场状态，
// 且未产生跨状态泄漏（RED 不会因六维 provider 被误判）。
func TestBearishUsesMarketState(t *testing.T) {
	p := &Planner{}
	// 状态缺失且无 provider：不得 panic，且视为非弱市
	if p.bearish() {
		t.Fatal("无市场状态且无 provider 时不应判为弱市")
	}
	p.lastMarketState = &port.MarketState{Regime: "bear"}
	if !p.bearish() {
		t.Fatal("Regime=bear 应判为弱市")
	}
	p.lastMarketState = &port.MarketState{Regime: "bear_range"}
	if !p.bearish() {
		t.Fatal("Regime=bear_range 应判为弱市")
	}
	// 市场状态优先于六维 provider：bull 状态下即便 provider 报退潮也不判弱市
	p.lastMarketState = &port.MarketState{Regime: "bull"}
	p.SetSixDimRegimeProvider(func() (string, float64, bool) { return "退潮风险", 0.05, true })
	if p.bearish() {
		t.Fatal("存在 bull 市场状态时不应被六维 provider 覆盖")
	}
}

// TestBearishFallsBackToSixDim 校验 fail-open 已修复：
// 选股市场状态缺失时，回退六维判势做保守判断，避免弱市现金保护静默失效。
func TestBearishFallsBackToSixDim(t *testing.T) {
	cases := []struct {
		name string
		tag  string
		rate float64
		ok   bool
		want bool
	}{
		{"provider不可用时不判弱市", "", 0, false, false},
		{"退潮风险为弱市", "退潮风险", 0.8, true, true},
		{"偏弱为弱市", "偏弱", 0.5, true, true},
		{"低仓位系数为弱市", "结构性震荡", 0.25, true, true},
		{"强势高仓位非弱市", "强势", 0.8, true, false},
		{"震荡中仓位非弱市", "结构性震荡", 0.5, true, false},
	}
	for _, c := range cases {
		p := &Planner{}
		tag, rate, ok := c.tag, c.rate, c.ok
		p.SetSixDimRegimeProvider(func() (string, float64, bool) { return tag, rate, ok })
		if got := p.bearish(); got != c.want {
			t.Errorf("%s: bearish() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMarketRegimeLabel 校验市场状态展示标签在状态缺失时回退六维判势。
func TestMarketRegimeLabel(t *testing.T) {
	p := &Planner{}
	if got := p.marketRegimeLabel(); got != "未知" {
		t.Errorf("无任何来源时标签 = %q, want 未知", got)
	}
	p.SetSixDimRegimeProvider(func() (string, float64, bool) { return "偏弱", 0.2, true })
	if got := p.marketRegimeLabel(); got != "六维:偏弱(0.20)" {
		t.Errorf("六维回退标签 = %q, want 六维:偏弱(0.20)", got)
	}
	// 市场状态存在时优先使用状态 Regime
	p.lastMarketState = &port.MarketState{Regime: "bear"}
	if got := p.marketRegimeLabel(); got != "bear" {
		t.Errorf("市场状态标签 = %q, want bear", got)
	}
}