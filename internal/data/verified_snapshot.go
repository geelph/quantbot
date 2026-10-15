// Package data verified_snapshot.go — 确定性验证快照。
//
// 对应 TradingAgents 的 market_data_validator.py（build_verified_market_snapshot）。
//
// 动机：LLM 可能在没核实数据的情况下编造精确数字（如"支撑位在 42.5"、"RSI 超卖"）。
// 本模块从 DuckDB 取指定 symbol 的 K 线，确定性计算常用指标（无 LLM），
// 渲染成 markdown 表格，作为 prompt 里的 ground truth。
//
// 关键约束：
//   - 所有计算都是纯数学，不涉及 LLM → 结果完全确定、可复现
//   - 日期严格 ≤ trade_date（防止前视偏差）
//   - 指标计算失败时输出 N/A 而非 panic（一个坏指标不应该让整条链断掉）
package data

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// VerifiedSnapshot 确定性验证快照
type VerifiedSnapshot struct {
	Symbol       string
	TradeDate    time.Time
	LatestDate   time.Time
	LatestOHLCV  OHLCVRow
	Indicators   map[string]float64
	RecentCloses []CloseRow
}

type OHLCVRow struct {
	Open, High, Low, Close, Volume float64
}

type CloseRow struct {
	Date  time.Time
	Close float64
}

// DefaultSnapshotIndicators 固定指标集合（和 TradingAgents 的 DEFAULT_SNAPSHOT_INDICATORS 对齐）
var DefaultSnapshotIndicators = []string{
	"ma_10", "ma_50", "ma_200",
	"rsi_14",
	"boll_mid", "boll_upper", "boll_lower",
	"macd", "macd_signal", "macd_hist",
	"atr_14",
}

// BuildVerifiedSnapshot 从 DuckDB 构建确定性验证快照。
// tradeDate 是分析截止日（K 线必须 ≤ 此日，否则前视偏差）。
// lookBackDays 取最近 N 根 K 线（默认 30）。
func (dm *DuckDBManager) BuildVerifiedSnapshot(
	ctx context.Context,
	symbol string,
	tradeDate time.Time,
	lookBackDays int,
) (*VerifiedSnapshot, error) {
	if dm == nil {
		return nil, fmt.Errorf("DuckDBManager is nil")
	}
	if lookBackDays <= 0 {
		lookBackDays = 30
	}

	// 取足够长的 K 线（指标需要预热窗口——MA200 需要 200 根，所以多取 250 根）
	bars, err := dm.GetKlineFromStock(ctx, symbol, lookBackDays+250)
	if err != nil {
		return nil, fmt.Errorf("fetch kline: %w", err)
	}
	if len(bars) < 20 {
		return nil, fmt.Errorf("snapshot: not enough bars (%d, need ≥20)", len(bars))
	}

	// 过滤 ≤ tradeDate 的行，按日期升序
	cutoff := tradeDate
	filtered := make([]KlineBarFromDuckDB, 0, len(bars))
	for _, b := range bars {
		if !b.Date.After(cutoff) {
			filtered = append(filtered, b)
		}
	}
	if len(filtered) < 20 {
		return nil, fmt.Errorf("snapshot: after date cutoff only %d bars left", len(filtered))
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Date.Before(filtered[j].Date) })

	// 最新一行
	latest := filtered[len(filtered)-1]

	// 确定性计算指标（全部纯数学，不涉及 LLM）
	indicators := computeAllIndicators(filtered)

	// 最近 N 根收盘价
	recentN := lookBackDays
	if recentN > len(filtered) {
		recentN = len(filtered)
	}
	recent := filtered[len(filtered)-recentN:]
	closes := make([]CloseRow, recentN)
	for i, b := range recent {
		closes[i] = CloseRow{Date: b.Date, Close: b.Close}
	}

	return &VerifiedSnapshot{
		Symbol:       symbol,
		TradeDate:    tradeDate,
		LatestDate:   latest.Date,
		LatestOHLCV:  OHLCVRow{Open: latest.Open, High: latest.High, Low: latest.Low, Close: latest.Close, Volume: latest.Volume},
		Indicators:   indicators,
		RecentCloses: closes,
	}, nil
}

// RenderMarkdown 渲染成 markdown 表格（和 TradingAgents 格式一致，供 LLM prompt 注入）
func (s *VerifiedSnapshot) RenderMarkdown() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## 确定性验证快照: %s\n\n", strings.ToUpper(s.Symbol)))
	sb.WriteString(fmt.Sprintf("- 请求分析日期: **%s**\n", s.TradeDate.Format("2006-01-02")))
	sb.WriteString(fmt.Sprintf("- 最新交易日 K 线: **%s**\n", s.LatestDate.Format("2006-01-02")))
	sb.WriteString("- 已排除请求日期之后的所有 K 线（无前视偏差）\n\n")

	// OHLCV 表
	sb.WriteString("### 最新 OHLCV\n\n")
	sb.WriteString("| Field | Value |\n|---|---:|\n")
	rows := [][2]string{
		{"Open", fmt.Sprintf("%.2f", s.LatestOHLCV.Open)},
		{"High", fmt.Sprintf("%.2f", s.LatestOHLCV.High)},
		{"Low", fmt.Sprintf("%.2f", s.LatestOHLCV.Low)},
		{"Close", fmt.Sprintf("%.2f", s.LatestOHLCV.Close)},
		{"Volume", fmt.Sprintf("%.0f", s.LatestOHLCV.Volume)},
	}
	for _, r := range rows {
		sb.WriteString(fmt.Sprintf("| %s | %s |\n", r[0], r[1]))
	}
	sb.WriteString("\n")

	// 指标表
	sb.WriteString("### 确定性计算的技术指标（纯数学，非 LLM）\n\n")
	sb.WriteString("| Indicator | Value |\n|---|---:|\n")
	for _, name := range DefaultSnapshotIndicators {
		if v, ok := s.Indicators[name]; ok && !math.IsNaN(v) {
			sb.WriteString(fmt.Sprintf("| %s | %.4f |\n", name, v))
		} else {
			sb.WriteString(fmt.Sprintf("| %s | N/A |\n", name))
		}
	}
	sb.WriteString("\n")

	// 最近收盘价
	sb.WriteString(fmt.Sprintf("### 最近 %d 根收盘价\n\n", len(s.RecentCloses)))
	sb.WriteString("| Date | Close |\n|---|---:|\n")
	for _, r := range s.RecentCloses {
		sb.WriteString(fmt.Sprintf("| %s | %.2f |\n", r.Date.Format("2006-01-02"), r.Close))
	}
	sb.WriteString("\n")

	// 关键约束语（TradingAgents 风格——让 LLM 知道"不能编造精确数字"）
	sb.WriteString(`### 真理来源约束

以上快照是确定性计算的 ground truth。请以此为真理来源处理 OHLCV、价格水平和指标值相关的所有精确数字声明。
- 如果其他工具输出与此冲突，**应标注差异而非编造一个调和数字**
- 不要声称"历史回测验证"、"支撑/阻力反弹"、"精确百分比波动"，除非直接被带有具体日期和价格的工具输出支持
- 这是确定性计算，不涉及 LLM —— 你不能质疑它的数值，只能质疑它的解读`)
	sb.WriteString("\n\n")

	return sb.String()
}

// === 指标计算 ===

// computeAllIndicators 计算全部默认指标。一个指标失败不影响其他。
func computeAllIndicators(bars []KlineBarFromDuckDB) map[string]float64 {
	closes := extractCloses(bars)
	highs := extractHighs(bars)
	lows := extractLows(bars)
	opens := extractOpens(bars)
	indicators := make(map[string]float64, len(DefaultSnapshotIndicators))

	// MA
	if v := simpleMA(closes, 10); !math.IsNaN(v) {
		indicators["ma_10"] = v
	}
	if v := simpleMA(closes, 50); !math.IsNaN(v) {
		indicators["ma_50"] = v
	}
	if v := simpleMA(closes, 200); !math.IsNaN(v) {
		indicators["ma_200"] = v
	}

	// RSI(14)
	if v := rsi(closes, 14); !math.IsNaN(v) {
		indicators["rsi_14"] = v
	}

	// Bollinger(20, 2)
	if mid, upper, lower := bollinger(closes, 20, 2); !math.IsNaN(mid) {
		indicators["boll_mid"] = mid
		indicators["boll_upper"] = upper
		indicators["boll_lower"] = lower
	}

	// MACD(12, 26, 9)
	if dif, dea, hist := macd(closes, 12, 26, 9); !math.IsNaN(dif) {
		indicators["macd"] = dif
		indicators["macd_signal"] = dea
		indicators["macd_hist"] = hist
	}

	// ATR(14)
	if v := atr(highs, lows, closes, opens, 14); !math.IsNaN(v) {
		indicators["atr_14"] = v
	}

	return indicators
}

func extractCloses(bars []KlineBarFromDuckDB) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}
func extractHighs(bars []KlineBarFromDuckDB) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.High
	}
	return out
}
func extractLows(bars []KlineBarFromDuckDB) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Low
	}
	return out
}
func extractOpens(bars []KlineBarFromDuckDB) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Open
	}
	return out
}

// simpleMA 简单移动平均
func simpleMA(data []float64, period int) float64 {
	if len(data) < period {
		return math.NaN()
	}
	sum := 0.0
	for i := len(data) - period; i < len(data); i++ {
		sum += data[i]
	}
	return sum / float64(period)
}

// rsi 相对强弱指数
func rsi(closes []float64, period int) float64 {
	if len(closes) < period+1 {
		return math.NaN()
	}
	gain, loss := 0.0, 0.0
	for i := len(closes) - period; i < len(closes); i++ {
		diff := closes[i] - closes[i-1]
		if diff > 0 {
			gain += diff
		} else {
			loss -= diff
		}
	}
	gain /= float64(period)
	loss /= float64(period)
	if loss == 0 {
		return 100.0
	}
	rs := gain / loss
	return 100.0 - 100.0/(1.0+rs)
}

// bollinger 布林带
func bollinger(closes []float64, period int, stdDevMul float64) (mid, upper, lower float64) {
	if len(closes) < period {
		return math.NaN(), math.NaN(), math.NaN()
	}
	mid = simpleMA(closes, period)
	if math.IsNaN(mid) {
		return math.NaN(), math.NaN(), math.NaN()
	}
	variance := 0.0
	for i := len(closes) - period; i < len(closes); i++ {
		variance += (closes[i] - mid) * (closes[i] - mid)
	}
	variance /= float64(period)
	stdDev := math.Sqrt(variance)
	upper = mid + stdDevMul*stdDev
	lower = mid - stdDevMul*stdDev
	return mid, upper, lower
}

// macd 指数平滑异同移动平均线
func macd(closes []float64, fastPeriod, slowPeriod, signalPeriod int) (dif, dea, hist float64) {
	if len(closes) < slowPeriod+signalPeriod {
		return math.NaN(), math.NaN(), math.NaN()
	}
	fastEMA := ema(closes, fastPeriod)
	slowEMA := ema(closes, slowPeriod)
	if math.IsNaN(fastEMA) || math.IsNaN(slowEMA) {
		return math.NaN(), math.NaN(), math.NaN()
	}
	dif = fastEMA - slowEMA

	// 计算全部 DIF 序列再算 DEA
	difSeries := make([]float64, len(closes))
	emaFast := 0.0
	emaSlow := 0.0
	f := 2.0 / float64(fastPeriod+1)
	s := 2.0 / float64(slowPeriod+1)
	initialized := false
	for i, c := range closes {
		if !initialized {
			emaFast = c
			emaSlow = c
			initialized = true
		} else {
			emaFast = c*f + emaFast*(1-f)
			emaSlow = c*s + emaSlow*(1-s)
		}
		difSeries[i] = emaFast - emaSlow
	}
	// 从第一个有效值开始算 DEA（前 slowPeriod 个 NaN）
	validDif := difSeries[slowPeriod:]
	if len(validDif) < signalPeriod {
		return dif, math.NaN(), math.NaN()
	}
	dea = ema(validDif, signalPeriod)
	if math.IsNaN(dea) {
		return dif, math.NaN(), math.NaN()
	}
	hist = 2.0 * (dif - dea)
	return dif, dea, hist
}

// ema 指数移动平均
func ema(data []float64, period int) float64 {
	if len(data) < period {
		return math.NaN()
	}
	alpha := 2.0 / float64(period+1)
	emaVal := data[len(data)-len(data)] // 第一个值
	for i := 1; i < len(data); i++ {
		emaVal = data[i]*alpha + emaVal*(1-alpha)
	}
	return emaVal
}

// atr 平均真实波幅
func atr(highs, lows, closes, opens []float64, period int) float64 {
	n := len(closes)
	if n < period+1 {
		return math.NaN()
	}
	trs := make([]float64, 0, n-1)
	for i := 1; i < n; i++ {
		tr := math.Max(highs[i]-lows[i],
			math.Max(math.Abs(highs[i]-closes[i-1]),
				math.Abs(lows[i]-closes[i-1])))
		_ = opens // 暂未使用 open，保留以备扩展
		trs = append(trs, tr)
	}
	if len(trs) < period {
		return math.NaN()
	}
	sum := 0.0
	for i := len(trs) - period; i < len(trs); i++ {
		sum += trs[i]
	}
	return sum / float64(period)
}
