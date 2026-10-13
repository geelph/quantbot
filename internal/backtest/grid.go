package backtest

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/quantpilot/quantpilot/internal/data"
	sixdim "github.com/quantpilot/quantpilot/internal/marketsixdim"
	"github.com/quantpilot/quantpilot/internal/tdx"
)

// 参数网格扫描：吸收 vectorbt bruteforce（参数网格枚举选优）的理念，并适配 A 股回测。
//
// 关键防前视设计（walk-forward 时间切分）：
//   - 参数不直接在整段历史上选优（那是变相用未来数据"拟合"参数，会夸大回测胜率）。
//   - 把过滤后的 K 线按时间切成两段：样本内（前 70%）用于给每组参数评分并选出最优；
//     样本外（后 30%）只用来验证这组被选中的参数，报告其真实表现。
//   - 于是"选参数"看到的是前 70% 数据，"报告绩效"用的是参数没见过但紧随其后的 30% 数据，
//     与 vectorbt 用时间切分避免 lookahead 的思路一致。
//
// 说明：本扫描是"分析/比优"工具，不写库、不改策略表；每一次网格组合都沿用 ExecNextOpen
// 撮合与三因子成本模型，保证与单次回测口径完全一致。

// GridObjObjective 网格扫描的选参目标。
type GridObjObjective string

const (
	GridObjSharpe GridObjObjective = "sharpe" // 样本内夏普比率优先（默认）
	GridObjReturn GridObjObjective = "return" // 样本内年化收益优先
)

// GridComboResult 单组参数的扫描结果（样本内指标，用于参选）。
type GridComboResult struct {
	Params   map[string]float64 `json:"params"`
	ParamMsg string             `json:"param_msg"` // 人类可读的参数展示，如 "快5/慢30"
	IsSharpe float64            `json:"is_sharpe"` // 样本内夏普
	IsReturn float64            `json:"is_return"` // 样本内年化收益(%)
	IsMaxDD  float64            `json:"is_max_dd"` // 样本内最大回撤(%)
	IsTrades int                `json:"is_trades"` // 样本内成交笔数
}

// GridScanResult 一次网格扫描的输出。
type GridScanResult struct {
	StrategyType string             `json:"strategy_type"`
	StockCode    string             `json:"stock_code"`
	Objective    GridObjObjective   `json:"objective"`
	CombosTested int                `json:"combos_tested"`
	TrainBars    int                `json:"train_bars"`
	TestBars     int                `json:"test_bars"`
	BestParams   map[string]float64 `json:"best_params"`
	BestParamMsg string             `json:"best_param_msg"`
	BestIsSharpe float64            `json:"best_is_sharpe"`
	BestIsReturn float64            `json:"best_is_return"`
	OosSharpe    float64            `json:"oos_sharpe"` // 最优参数在样本外（30%）的真实表现
	OosReturn    float64            `json:"oos_return"`
	OosMaxDD     float64            `json:"oos_max_dd"`
	OosTrades    int                `json:"oos_trades"`
	Ranked       []GridComboResult  `json:"ranked"` // 按目标降序
	Method       string             `json:"method"` // 防前视方法说明
	// RegimeOOS 分市场环境（牛/熊/震荡）的样本外绩效：暴露"策略在弱市是否仍有效"，
	// 仅取被选中最优参数的样本外表现；环境过短（<15根）的区间不参与统计。
	RegimeOOS []RegimeOOSResult `json:"regime_oos"`
	// RegimeSource 环境标签来源说明（六维判势 / 沪深300 trailing 回退）
	RegimeSource string `json:"regime_source"`
}

// RegimeOOSResult 单一市场环境（牛/熊/震荡）下的样本外绩效。
type RegimeOOSResult struct {
	Regime string  `json:"regime"`
	Bars   int     `json:"bars"`
	Sharpe float64 `json:"sharpe"`
	Return float64 `json:"return"`
	MaxDD  float64 `json:"max_dd"`
	Trades int     `json:"trades"`
}

// runComboOnSegment 在给定 K 线段上用指定参数跑一次回测，返回样本内关键指标。
// 传入 nil 参数时退化为"无参网格"（用该策略默认参数），便于扫描/单跑复用。
func runComboOnSegment(strategyType string, params map[string]float64, seg []tdx.KlineBar, req *RunBacktestRequest, dm *data.DuckDBManager) *BacktestResult {
	strategy, err := NewStrategy(strategyType, params)
	if err != nil {
		return &BacktestResult{BarsCount: len(seg)}
	}
	engine := buildBacktestEngine(seg, strategy, req, dm)
	return engine.Run()
}

// RunBacktestGrid 对某策略做参数网格扫描：样本内选优 + 样本外验证（含成交笔数兜底）。
// objective 可选 "sharpe" 或 "return"；grid 为 nil 时使用 DefaultParamGrid。
func (s *Service) RunBacktestGrid(req *RunBacktestRequest, objective string, grid []map[string]float64) (*GridScanResult, error) {
	if req == nil {
		return nil, fmt.Errorf("网格扫描请求为空")
	}
	if grid == nil {
		grid = DefaultParamGrid(req.StrategyType)
	}
	if len(grid) == 0 {
		return nil, fmt.Errorf("策略 %s 未提供可扫描的参数网格（暂不支持参数扫描）", req.StrategyType)
	}
	obj := GridObjObjective(objective)
	if obj != GridObjReturn {
		obj = GridObjSharpe // 统一默认：样本内夏普优先
	}
	// 与单次回测同一数据入口（自动识别市场/优先DuckDB/回退TDX/日期过滤）
	bars, err := s.fetchBars(req)
	if err != nil {
		return nil, err
	}
	return scanGrid(grid, bars, req, s.duckdbMgr, obj)
}

// scanGrid 网格扫描的纯逻辑（无网络/DB 依赖，便于自测）：
// walk-forward 时间切分（前70%评分选参、后30%验证）→ 逐组回测 → 按目标排序 → 最优组在样本外验证。
func scanGrid(grid []map[string]float64, bars []tdx.KlineBar, req *RunBacktestRequest, dm *data.DuckDBManager, obj GridObjObjective) (*GridScanResult, error) {
	if len(bars) < 40 {
		return nil, fmt.Errorf("网格扫描至少需要40根K线用于样本内/外切分，当前仅%d条", len(bars))
	}
	// 防御：组合数过多时截断，避免长时间无反馈
	if len(grid) > 200 {
		log.Printf("[BacktestGrid] 组合数为 %d 超过上限200，截断为前200组", len(grid))
		grid = grid[:200]
	}

	// walk-forward 时间切分：样本内前70%，样本外后30%
	trainEnd := len(bars) * 7 / 10
	train, test := bars[:trainEnd], bars[trainEnd:]

	// 并行逐组回测：共享 train 只读数据，仅按下标写入结果互不冲突，财务提供者走 *sql.DB 并发安全。
	// 有界 worker 池控制并发，避免一次性协程大海创造成本与 CPU 峰值；进度日志保留去重防刷屏。
	workerCount := len(grid)
	if workerCount > 8 {
		workerCount = 8
	}
	type gridTask struct {
		idx    int
		params map[string]float64
	}
	results := make([]GridComboResult, len(grid))
	jobCh := make(chan gridTask)
	var wg sync.WaitGroup
	var done int32
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobCh {
				r := runComboOnSegment(req.StrategyType, t.params, train, req, dm)
				results[t.idx] = GridComboResult{
					Params:   t.params,
					ParamMsg: paramLabel(req.StrategyType, t.params),
					IsSharpe: r.SharpeRatio,
					IsReturn: r.AnnualReturn,
					IsMaxDD:  r.MaxDrawdown,
					IsTrades: r.TotalTrades,
				}
				n := atomic.AddInt32(&done, 1)
				if n%50 == 0 || n == int32(len(grid)) {
					log.Printf("[BacktestGrid] 扫描进度 %d/%d", n, len(grid))
				}
			}
		}()
	}
	for i, params := range grid {
		jobCh <- gridTask{idx: i, params: params}
	}
	close(jobCh)
	wg.Wait()

	// 按目标排序（降序）。叠加"至少1笔成交"作为有效性前提，防止选出0成交的虚假最优。
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if (a.IsTrades == 0) != (b.IsTrades == 0) {
			return a.IsTrades > 0
		}
		if obj == GridObjReturn {
			return a.IsReturn > b.IsReturn
		}
		return a.IsSharpe > b.IsSharpe
	})

	if len(results) == 0 {
		return nil, fmt.Errorf("扫描未产生任何可用组合")
	}
	best := results[0]
	oos := runComboOnSegment(req.StrategyType, best.Params, test, req, dm)
	// 分市场环境（牛/熊/震荡）样本外验证：用真实市场环境数据给样本外K线分层，分别回测最优参数。
	regimeOOS, regimeSource := computeRegimeOOS(req.StrategyType, best.Params, test, req, dm)

	res := &GridScanResult{
		StrategyType: req.StrategyType,
		StockCode:    req.StockCode,
		Objective:    obj,
		CombosTested: len(results),
		TrainBars:    len(train),
		TestBars:     len(test),
		BestParams:   best.Params,
		BestParamMsg: best.ParamMsg,
		BestIsSharpe: best.IsSharpe,
		BestIsReturn: best.IsReturn,
		OosSharpe:    oos.SharpeRatio,
		OosReturn:    oos.AnnualReturn,
		OosMaxDD:     oos.MaxDrawdown,
		OosTrades:    oos.TotalTrades,
		Ranked:       results,
		Method:       "walk-forward 时间切分：参数在样本内(前70%)评分选优，样本外(后30%)验证，避免用全样本选参产生前视/过拟合；样本外再按市场环境(牛/熊/震荡)分层验证弱市鲁棒性。",
		RegimeOOS:    regimeOOS,
		RegimeSource: regimeSource,
	}
	log.Printf("[BacktestGrid] strategy=%s combos=%d best=%s isSharpe=%.3f oosSharpe=%.3f oosReturn=%.1f%% oosDD=%.1f%% testBars=%d regimes=%d(%s)",
		req.StrategyType, res.CombosTested, best.ParamMsg, best.IsSharpe, res.OosSharpe, res.OosReturn, res.OosMaxDD, res.TestBars, len(regimeOOS), regimeSource)
	return res, nil
}

// simpleRegimeLabel 把统一市场环境（sixdim.SimpleRegime）归一为中文「牛/熊/震荡」。
func simpleRegimeLabel(r sixdim.SimpleRegime) string {
	switch r {
	case sixdim.SimpleBullish:
		return "牛"
	case sixdim.SimpleBearish:
		return "熊"
	default:
		return "震荡"
	}
}

// regimeLabelsForBars 为样本外每根K线打市场环境标签（牛/熊/震荡）。
// 优先使用 market_sixdim_daily 的每日判势（统一口径 sixdim.ToSimpleRegime 归一，避免四套口径割裂）；
// 六维历史覆盖不足（<50%）时回退用沪深300 trailing 20 日收益判定（真实指数数据）。
// 返回标签切片与来源说明；两者都不可用时返回 nil。
func regimeLabelsForBars(bars []tdx.KlineBar, dm *data.DuckDBManager) ([]string, string) {
	if len(bars) == 0 || dm == nil || !dm.HasStockDB() {
		return nil, ""
	}
	trimDate := func(d string) string {
		if len(d) > 10 {
			return d[:10]
		}
		return d
	}
	// 1) 六维判势（真实落库数据）
	if rows, err := data.GetMarketSixDimRows(context.Background(), dm, 5000); err == nil && len(rows) > 0 {
		byDate := make(map[string]string, len(rows))
		for _, row := range rows {
			byDate[trimDate(row.TradeDate)] = simpleRegimeLabel(sixdim.ToSimpleRegime(row.MarketTag, row.PositionRate))
		}
		labels := make([]string, len(bars))
		hit := 0
		for i, b := range bars {
			if l, ok := byDate[trimDate(b.Date)]; ok {
				labels[i] = l
				hit++
			} else {
				labels[i] = "震荡" // 缺失按震荡处理，保守，不夸大牛/熊表现
			}
		}
		if hit*2 >= len(bars) {
			return labels, fmt.Sprintf("六维判势 market_sixdim_daily（覆盖 %d/%d 根）", hit, len(bars))
		}
	}
	// 2) 回退：沪深300 trailing 20 日收益判定
	bench := loadBenchmarkBars(dm, BenchmarkIndexCode, BenchmarkIndexName, len(bars)+30, "", "")
	if len(bench) < benchmarkRegimeWindow+2 {
		return nil, ""
	}
	closeByDate := make(map[string]float64, len(bench))
	for _, b := range bench {
		if b.Close > 0 {
			closeByDate[b.Date] = b.Close
		}
	}
	series := make([]float64, len(bars))
	last := 0.0
	for i, b := range bars {
		if c, ok := closeByDate[trimDate(b.Date)]; ok && c > 0 {
			last = c
		}
		series[i] = last
	}
	labels := make([]string, len(bars))
	for i := range bars {
		if i < benchmarkRegimeWindow || series[i] <= 0 || series[i-benchmarkRegimeWindow] <= 0 {
			labels[i] = "震荡"
			continue
		}
		r := series[i]/series[i-benchmarkRegimeWindow] - 1
		switch {
		case r > 0.03:
			labels[i] = "牛"
		case r < -0.03:
			labels[i] = "熊"
		default:
			labels[i] = "震荡"
		}
	}
	return labels, "沪深300 trailing 20 日收益判定（六维历史覆盖不足）"
}

// computeRegimeOOS 分市场环境（牛/熊/震荡）的样本外验证：
// 把样本外K线按环境标签聚合为连续子区间，分别用被选中的最优参数回测，暴露"策略在弱市是否仍有效"。
// 局限：同环境子区间在原时间轴上并不连续，拼接回测会在边界处存在跳空，仅用于横向比较各环境表现。
func computeRegimeOOS(strategyType string, params map[string]float64, test []tdx.KlineBar, req *RunBacktestRequest, dm *data.DuckDBManager) ([]RegimeOOSResult, string) {
	if len(test) < 20 {
		return nil, ""
	}
	labels, source := regimeLabelsForBars(test, dm)
	if labels == nil {
		return nil, ""
	}
	var out []RegimeOOSResult
	for start := 0; start < len(test); {
		cur := labels[start]
		end := start
		for end+1 < len(test) && labels[end+1] == cur {
			end++
		}
		if seg := test[start : end+1]; len(seg) >= 15 {
			r := runComboOnSegment(strategyType, params, seg, req, dm)
			out = append(out, RegimeOOSResult{
				Regime: cur,
				Bars:   len(seg),
				Sharpe: r.SharpeRatio,
				Return: r.AnnualReturn,
				MaxDD:  r.MaxDrawdown,
				Trades: r.TotalTrades,
			})
		}
		start = end + 1
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, source
}

// paramLabel 把一组参数渲染成人类可读、可复制的字符串（如 "快5/慢30"）。
func paramLabel(strategyType string, params map[string]float64) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s%.0f", shortParamName(k), params[k]))
	}
	return strings.Join(parts, "/")
}

// shortParamName 参数名缩写，保持展示紧凑。
func shortParamName(k string) string {
	aliases := map[string]string{
		"FastPeriod":    "快",
		"SlowPeriod":    "慢",
		"Period":        "周期",
		"K":             "K",
		"OverSold":      "超卖",
		"Overbought":    "超买",
		"KPeriod":       "K周期",
		"DPeriod":       "D周期",
		"JOverSold":     "J超卖",
		"JOverbought":   "J超买",
		"EntryPeriod":   "入",
		"ExitPeriod":    "出",
		"Multiplier":    "倍",
		"BuyThreshold":  "买阈",
		"SellThreshold": "卖阈",
	}
	if v, ok := aliases[k]; ok {
		return v
	}
	return k
}
