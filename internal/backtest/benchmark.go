package backtest

import (
	"context"
	"log"

	"github.com/quantpilot/quantpilot/internal/data"
)

// 本文件为回测补充「基准对比 + 分市场环境绩效」：
// 既有的回测只有绝对收益/夏普/回撤，缺少「相对基准」与「分牛/熊/震荡区间的表现」，
// 无法回答"策略的收益来自选股能力还是市场 beta"以及"策略在弱市是否抗跌"。
//
// 基准序列由调用方通过 BacktestEngine.BenchmarkBars 注入（真实指数K线，严禁伪造）；
// 缺失或与回测区间无法对齐时，回测结果不产出该块（BacktestResult.Benchmark == nil）。

// BenchmarkIndexCode 默认基准指数（沪深300，DuckDB symbol 前缀）
const BenchmarkIndexCode = "sh000300"

// BenchmarkIndexName 默认基准指数中文名
const BenchmarkIndexName = "沪深300"

// loadBenchmarkBars 从 DuckDB 读取指数K线作为回测基准（stock.ohlc 真实数据，严禁伪造）。
// indexCode 形如 "sh000300"；days 为最多取多少根（建议 ≥ 回测 bar 数）。
// 返回按日期升序、区间 [startDate, endDate] 内的 BarData；数据不足 20 根返回 nil。
func loadBenchmarkBars(dm *data.DuckDBManager, indexCode, name string, days int, startDate, endDate string) []BarData {
	if dm == nil {
		return nil
	}
	raw, err := dm.GetIndexKlineFromStock(context.Background(), indexCode, days)
	if err != nil || len(raw) < 20 {
		log.Printf("[Backtest] 基准 %s(%s) 数据不可用: err=%v, n=%d", name, indexCode, err, len(raw))
		return nil
	}
	// DuckDB 返回按日期倒序，翻转为升序后按区间过滤
	bars := make([]BarData, 0, len(raw))
	for i := len(raw) - 1; i >= 0; i-- {
		b := raw[i]
		d := b.Date.Format("2006-01-02")
		if startDate != "" && d < startDate {
			continue
		}
		if endDate != "" && d > endDate {
			continue
		}
		bars = append(bars, BarData{
			Date:   d,
			Open:   b.Open,
			High:   b.High,
			Low:    b.Low,
			Close:  b.Close,
			Volume: int64(b.Volume),
			Amount: b.Amount,
		})
	}
	if len(bars) < 20 {
		log.Printf("[Backtest] 基准 %s(%s) 区间 [%s,%s] 内仅 %d 根，跳过基准对比", name, indexCode, startDate, endDate, len(bars))
		return nil
	}
	return bars
}

// BenchmarkMetrics 基准对比与分区间（牛/熊/震荡）绩效
type BenchmarkMetrics struct {
	BenchmarkName   string          `json:"benchmarkName"`
	BenchmarkReturn float64         `json:"benchmarkReturn"` // 区间基准累计收益
	StrategyReturn  float64         `json:"strategyReturn"`  // 区间策略累计收益
	ExcessReturn    float64         `json:"excessReturn"`    // 超额收益 = 策略收益 - 基准收益
	Alpha           float64         `json:"alpha"`           // 年化 Alpha（相对基准）
	Beta            float64         `json:"beta"`            // 组合 Beta
	Segments        []RegimeSegment `json:"segments"`        // 分环境绩效
}

// RegimeSegment 单一市场环境区间的绩效
type RegimeSegment struct {
	Regime          string  `json:"regime"` // 牛/熊/震荡
	StartDate       string  `json:"startDate"`
	EndDate         string  `json:"endDate"`
	Bars            int     `json:"bars"`
	StrategyReturn  float64 `json:"strategyReturn"`
	BenchmarkReturn float64 `json:"benchmarkReturn"`
	ExcessReturn    float64 `json:"excessReturn"`
	MaxDrawdown     float64 `json:"maxDrawdown"`
	Sharpe          float64 `json:"sharpe"`
}

// benchmarkRegimeWindow 判定市场环境所用的基准 trailing 收益窗口（交易日）
const benchmarkRegimeWindow = 20

// computeBenchmarkMetrics 计算基准对比与分区间绩效。
// equity 与 dates 按 bar 对齐；benchBars 为基准（指数）K线，按日期匹配到回测 bar。
// 分环境用基准自身 trailing 20 日收益判定：>+3% 牛、<-3% 熊、其余震荡，
// 再把连续同态 bar 聚合为区间，逐区间统计策略/基准收益、超额、最大回撤与夏普。
func computeBenchmarkMetrics(equity []float64, dates []string, benchBars []BarData, name string) *BenchmarkMetrics {
	n := len(equity)
	if n < 2 || len(dates) != n || len(benchBars) == 0 {
		return nil
	}
	// 基准按日期对齐到回测 bar：缺失日沿用前值（保持长度一致）；首值缺失则判定无法对齐
	closeByDate := make(map[string]float64, len(benchBars))
	for _, b := range benchBars {
		if b.Date != "" && b.Close > 0 {
			closeByDate[b.Date] = b.Close
		}
	}
	bench := make([]float64, n)
	last := 0.0
	for i, d := range dates {
		if c, ok := closeByDate[d]; ok && c > 0 {
			last = c
		}
		bench[i] = last
	}
	if bench[0] <= 0 {
		return nil // 基准与回测区间无法对齐
	}

	m := &BenchmarkMetrics{BenchmarkName: name}
	m.BenchmarkReturn = bench[n-1]/bench[0] - 1
	if equity[0] > 0 {
		m.StrategyReturn = equity[n-1]/equity[0] - 1
		m.ExcessReturn = m.StrategyReturn - m.BenchmarkReturn
	}

	// Beta / Alpha（基于日收益，年化 ×250）
	stratRet := make([]float64, 0, n-1)
	benchRet := make([]float64, 0, n-1)
	for i := 1; i < n; i++ {
		if equity[i-1] > 0 && bench[i-1] > 0 {
			stratRet = append(stratRet, equity[i]/equity[i-1]-1)
			benchRet = append(benchRet, bench[i]/bench[i-1]-1)
		}
	}
	if v := variance(benchRet); v > 1e-12 {
		m.Beta = covariance(stratRet, benchRet) / v
	}
	m.Alpha = mean(stratRet)*250 - m.Beta*mean(benchRet)*250

	// 分环境聚合（连续同态 bar 合并为一个区间）
	label := func(i int) string {
		if i < benchmarkRegimeWindow {
			return "震荡"
		}
		r := bench[i]/bench[i-benchmarkRegimeWindow] - 1
		switch {
		case r > 0.03:
			return "牛"
		case r < -0.03:
			return "熊"
		default:
			return "震荡"
		}
	}
	for start := 0; start < n; {
		cur := label(start)
		end := start
		for end+1 < n && label(end+1) == cur {
			end++
		}
		seg := RegimeSegment{
			Regime:    cur,
			StartDate: dates[start],
			EndDate:   dates[end],
			Bars:      end - start + 1,
		}
		base := start - 1
		if base < 0 {
			base = 0
		}
		if equity[base] > 0 && bench[base] > 0 {
			seg.StrategyReturn = equity[end]/equity[base] - 1
			seg.BenchmarkReturn = bench[end]/bench[base] - 1
			seg.ExcessReturn = seg.StrategyReturn - seg.BenchmarkReturn
		}
		seg.MaxDrawdown = calculateMaxDrawdown(equity[start : end+1])
		seg.Sharpe = calculateSharpeRatio(equity[start : end+1])
		m.Segments = append(m.Segments, seg)
		start = end + 1
	}
	return m
}

// mean 求均值（空切片返回 0）
func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// variance 求样本方差（分母 n-1）
func variance(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	mu := mean(xs)
	s := 0.0
	for _, x := range xs {
		d := x - mu
		s += d * d
	}
	return s / float64(len(xs)-1)
}

// covariance 求两组等长序列的样本协方差（分母 n-1）
func covariance(xs, ys []float64) float64 {
	n := len(xs)
	if n < 2 || n != len(ys) {
		return 0
	}
	mx, my := mean(xs), mean(ys)
	s := 0.0
	for i := 0; i < n; i++ {
		s += (xs[i] - mx) * (ys[i] - my)
	}
	return s / float64(n-1)
}
