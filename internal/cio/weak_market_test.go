package cio

import (
	"math"
	"testing"
	"time"

	sixdim "github.com/quantpilot/quantpilot/internal/marketsixdim"
)

func cioApprox(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

// TestSixDimTargetExposure 校验六维判势 → 目标权益敞口的推导：
// 以仓位系数为基准，情绪/资金维度极端恶化时只降不升。
func TestSixDimTargetExposure(t *testing.T) {
	cases := []struct {
		name string
		r    *sixdim.MarketReport
		want float64
	}{
		{"nil时保守默认0.3", nil, 0.3},
		{"正常取仓位系数", &sixdim.MarketReport{PositionRate: 0.7}, 0.7},
		{"上界截断为1", &sixdim.MarketReport{PositionRate: 1.5}, 1.0},
		{"下界截断为0", &sixdim.MarketReport{PositionRate: -0.2}, 0.0},
		{"情绪冰点收紧到0.2", &sixdim.MarketReport{PositionRate: 0.7, DimScores: map[string]float64{"sentiment": 20}}, 0.2},
		{"情绪恰好25收紧到0.2", &sixdim.MarketReport{PositionRate: 0.7, DimScores: map[string]float64{"sentiment": 25}}, 0.2},
		{"情绪非极端不收紧", &sixdim.MarketReport{PositionRate: 0.7, DimScores: map[string]float64{"sentiment": 26}}, 0.7},
		{"资金恶化收紧到0.3", &sixdim.MarketReport{PositionRate: 0.7, DimScores: map[string]float64{"capital": 10}}, 0.3},
		{"资金非极端不收紧", &sixdim.MarketReport{PositionRate: 0.7, DimScores: map[string]float64{"capital": 40}}, 0.7},
		{"只降不升:低仓位不被抬高", &sixdim.MarketReport{PositionRate: 0.15, DimScores: map[string]float64{"sentiment": 20}}, 0.15},
		{"情绪与资金同时恶化取更严者", &sixdim.MarketReport{PositionRate: 0.9, DimScores: map[string]float64{"sentiment": 10, "capital": 10}}, 0.2},
	}
	for _, c := range cases {
		if got := sixDimTargetExposure(c.r); !cioApprox(got, c.want, 1e-12) {
			t.Errorf("%s: sixDimTargetExposure = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSixDimDeleverageReduceRatio 校验主动降仓的纯计算：
// 仅当敞口高于目标 5 个百分点才动作，且单次最多收敛差额的一半。
func TestSixDimDeleverageReduceRatio(t *testing.T) {
	cases := []struct {
		name                     string
		curMarket, total, target float64
		want                     float64
	}{
		{"满仓对目标0.3:收敛差额一半", 100000, 100000, 0.3, 0.35},
		{"半仓对目标0.2", 50000, 100000, 0.2, 0.3},
		{"敞口在目标+5pp内不动作", 32000, 100000, 0.3, 0},
		{"敞口恰好等于目标+5pp不动作", 35000, 100000, 0.3, 0},
		{"目标0时满仓收敛一半", 100000, 100000, 0, 0.5},
		{"目标为负按0处理", 100000, 100000, -1, 0.5},
		{"目标>1截断为1:满仓不操作", 100000, 100000, 2, 0},
		{"无持仓市值返回0", 0, 100000, 0.3, 0},
		{"总资产非法返回0", 100000, 0, 0.3, 0},
	}
	for _, c := range cases {
		if got := sixDimDeleverageReduceRatio(c.curMarket, c.total, c.target); !cioApprox(got, c.want, 1e-12) {
			t.Errorf("%s: sixDimDeleverageReduceRatio(%v,%v,%v) = %v, want %v",
				c.name, c.curMarket, c.total, c.target, got, c.want)
		}
	}
}

// TestSixDimDeleverageAllowedThrottle 校验降仓节流：首次放行、30 分钟内拦截、超时后重新放行。
func TestSixDimDeleverageAllowedThrottle(t *testing.T) {
	eng := newTestCIOEngine()
	if !eng.sixDimDeleverageAllowed() {
		t.Fatal("首次降仓应放行")
	}
	if eng.sixDimDeleverageAllowed() {
		t.Fatal("30 分钟内第二次降仓应被节流")
	}
	// 手动回拨上次降仓时间，模拟超过节流窗口
	eng.deleverageMu.Lock()
	eng.lastDeleverageAt = time.Now().Add(-31 * time.Minute)
	eng.deleverageMu.Unlock()
	if !eng.sixDimDeleverageAllowed() {
		t.Fatal("超过 30 分钟后应重新放行")
	}
}

// TestEffectiveStopLossPctTightensInWeakMarket 校验统一止损阈值源：
// 无弱市数据时基准 5%，弱市时收紧到 3.5%。
func TestEffectiveStopLossPctTightensInWeakMarket(t *testing.T) {
	eng := newTestCIOEngine()
	if got := eng.effectiveStopLossPct(); got != 0.05 {
		t.Errorf("非弱市止损阈值 = %v, want 0.05", got)
	}
	eng.sixdimCacheMu.Lock()
	eng.sixdimCacheBearish = true
	eng.sixdimCacheAt = time.Now()
	eng.sixdimCacheMu.Unlock()
	if got := eng.effectiveStopLossPct(); got != 0.035 {
		t.Errorf("弱市止损阈值 = %v, want 0.035", got)
	}
}

// TestMarketRegimeBearishUsesCache 校验弱市判定：duckDB 缺失时判为非弱市，
// 且缓存有效期内直接复用缓存值（不再触发数据查询）。
func TestMarketRegimeBearishUsesCache(t *testing.T) {
	eng := newTestCIOEngine() // duckDB 为 nil
	if eng.marketRegimeBearish() {
		t.Fatal("无六维数据时不应判为弱市")
	}
	eng.sixdimCacheMu.Lock()
	eng.sixdimCacheBearish = true
	eng.sixdimCacheAt = time.Now()
	eng.sixdimCacheMu.Unlock()
	if !eng.marketRegimeBearish() {
		t.Fatal("缓存有效期内应直接返回缓存的弱市判定")
	}
}

// TestIndexAttributionGuards 校验指数归因在依赖缺失/样本不足时的兜底（返回 nil/false，不 panic）。
func TestIndexAttributionGuards(t *testing.T) {
	eng := newTestCIOEngine() // duckDB 为 nil
	if got := eng.computeIndexAttribution([]float64{0.01, 0.02}); got != nil {
		t.Errorf("duckDB 缺失时应返回 nil, got %+v", got)
	}
	if got := eng.computeIndexAttribution([]float64{0.01}); got != nil {
		t.Errorf("收益序列过短应返回 nil, got %+v", got)
	}
	if _, ok := eng.indexIntervalReturn(20); ok {
		t.Error("duckDB 缺失时 indexIntervalReturn 应返回 false")
	}
}