package backtest

import (
	"fmt"
	"math"
	"testing"

	sixdim "github.com/quantpilot/quantpilot/internal/marketsixdim"
)

func bmApprox(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

// TestMeanVarianceCovariance 校验统计基元（空/短序列兜底 + 常规取值）。
func TestMeanVarianceCovariance(t *testing.T) {
	if got := mean(nil); got != 0 {
		t.Errorf("mean(nil) = %v, want 0", got)
	}
	xs := []float64{2, 4, 6}
	if got := mean(xs); !bmApprox(got, 4, 1e-12) {
		t.Errorf("mean = %v, want 4", got)
	}
	if got := variance([]float64{1}); got != 0 {
		t.Errorf("variance(单元素) = %v, want 0", got)
	}
	// ((-2)^2 + 0 + 2^2) / (3-1) = 4
	if got := variance(xs); !bmApprox(got, 4, 1e-12) {
		t.Errorf("variance = %v, want 4", got)
	}
	if got := covariance(xs, xs); !bmApprox(got, 4, 1e-12) {
		t.Errorf("covariance(x,x) 应等于方差, got %v", got)
	}
	if got := covariance(xs, []float64{1, 2}); got != 0 {
		t.Errorf("长度不一致应返回0, got %v", got)
	}
}

// TestComputeBenchmarkMetricsBasic 策略与基准同步时：超额≈0、Beta≈1，
// 且分环境区间应完整覆盖所有 bar。
func TestComputeBenchmarkMetricsBasic(t *testing.T) {
	n := 30
	dates := make([]string, n)
	equity := make([]float64, n)
	benchBars := make([]BarData, n)
	for i := 0; i < n; i++ {
		dates[i] = fmt.Sprintf("2020-01-%02d", i+1)
		equity[i] = 100 + float64(i)
		benchBars[i] = BarData{Date: dates[i], Close: 100 + float64(i)}
	}

	m := computeBenchmarkMetrics(equity, dates, benchBars, "沪深300")
	if m == nil {
		t.Fatal("computeBenchmarkMetrics 返回 nil，期望有效结果")
	}
	if m.BenchmarkName != "沪深300" {
		t.Errorf("基准名 = %q, want 沪深300", m.BenchmarkName)
	}
	if !bmApprox(m.BenchmarkReturn, 0.29, 1e-9) {
		t.Errorf("基准区间收益 = %v, want 0.29", m.BenchmarkReturn)
	}
	if !bmApprox(m.StrategyReturn, m.BenchmarkReturn, 1e-9) {
		t.Errorf("策略与基准同步时收益应相等: %v vs %v", m.StrategyReturn, m.BenchmarkReturn)
	}
	if !bmApprox(m.ExcessReturn, 0, 1e-9) {
		t.Errorf("超额收益 = %v, want 0", m.ExcessReturn)
	}
	if !bmApprox(m.Beta, 1, 1e-6) {
		t.Errorf("Beta = %v, want 1", m.Beta)
	}
	totalBars := 0
	for _, s := range m.Segments {
		totalBars += s.Bars
		if s.Regime == "" {
			t.Errorf("区间缺失环境标签: %+v", s)
		}
	}
	if totalBars != n {
		t.Errorf("分环境区间总 bar 数 = %d, want %d", totalBars, n)
	}
}

// TestComputeBenchmarkMetricsGuards 校验各类不可用输入的兜底：返回 nil 而非 panic 或伪造数据。
func TestComputeBenchmarkMetricsGuards(t *testing.T) {
	// 长度不足
	if got := computeBenchmarkMetrics([]float64{1}, []string{"2020-01-01"}, []BarData{{Date: "2020-01-01", Close: 1}}, "x"); got != nil {
		t.Errorf("样本不足应返回 nil, got %+v", got)
	}
	// dates 与 equity 长度不一致
	if got := computeBenchmarkMetrics([]float64{1, 2, 3}, []string{"2020-01-01"}, []BarData{{Date: "2020-01-01", Close: 1}}, "x"); got != nil {
		t.Errorf("日期长度不匹配应返回 nil, got %+v", got)
	}
	// 无基准数据
	if got := computeBenchmarkMetrics([]float64{1, 2, 3}, []string{"2020-01-01", "2020-01-02", "2020-01-03"}, nil, "x"); got != nil {
		t.Errorf("无基准应返回 nil, got %+v", got)
	}
	// 首日无法与基准对齐
	equity := []float64{100, 101, 102}
	dates := []string{"2020-01-02", "2020-01-03", "2020-01-04"}
	bench := []BarData{{Date: "2021-01-01", Close: 1}}
	if got := computeBenchmarkMetrics(equity, dates, bench, "x"); got != nil {
		t.Errorf("基准与回测区间无法对齐应返回 nil, got %+v", got)
	}
}

// TestSimpleRegimeLabel 校验统一市场环境到中文「牛/熊/震荡」的映射。
func TestSimpleRegimeLabel(t *testing.T) {
	cases := map[sixdim.SimpleRegime]string{
		sixdim.SimpleBullish: "牛",
		sixdim.SimpleBearish: "熊",
		sixdim.SimpleNeutral: "震荡",
	}
	for in, want := range cases {
		if got := simpleRegimeLabel(in); got != want {
			t.Errorf("simpleRegimeLabel(%s) = %q, want %q", in, got, want)
		}
	}
}