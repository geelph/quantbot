// Package factors 集中定义因子元数据字典：中文名、描述、数值口径。
// 对应 AlphaLom 的 factor_desc.py（ALL_FACTOR_FIELDS + 因子层级分析），
// 但 QuantBot 用 DuckDB + Go 实现，因子由 ProFactorEngine 直接计算，
// 所以字典只做"元数据集中管理"——不参与因子计算，只供前端展示和 API 返回元信息。
//
// 注意：FactorCategory 常量（CategoryMomentum/CategoryValue 等）已在 engine.go 定义，
// 本文件不重复，只定义 NumericScale 和 AllFactors 元数据表。
package factors

// NumericScale 因子数值口径（告诉前端和使用者怎么解释这个因子的输出）
type NumericScale string

const (
	Scale0To100     NumericScale = "0-100分"  // 归一化打分（screener 输出）
	ScalePercent    NumericScale = "小数%形式"   // 0.03 = 3%，如 mom_* 收益率
	ScaleHundredPct NumericScale = "0-100%"  // 0.03 显示为 3
	ScaleRawRatio   NumericScale = "原始比率"    // 如 PE=15.3、PB=1.2
	ScaleZScore     NumericScale = "z-score" // 标准化残差，0=中性，±2=极端
	ScaleTurnover   NumericScale = "换手率"     // 小数形式，0.05 = 5%
)

// FactorMeta 单个因子的元数据
type FactorMeta struct {
	ID             string       // 内部 ID
	Name           string       // 中文展示名
	Category       string       // 分类（复用 engine.go 的 Category* 常量，string 类型）
	Scale          NumericScale // 数值口径
	Description    string       // 简要描述（≤60 字）
	HigherIsBetter bool         // 分数越高是否越好
}

// AllFactors 全部 8 个 Pro 因子的元数据字典（对标 AlphaLom 的 107 因子库，但 QuantBot 用 Barra 风格 8 因子更精简）
var AllFactors = map[string]FactorMeta{
	"value": {
		ID:             "value",
		Name:           "价值",
		Category:       CategoryValue,
		Scale:          Scale0To100,
		Description:    "低估值（PE/PB/股息率），数值口径 0-100 分",
		HigherIsBetter: true,
	},
	"quality": {
		ID:             "quality",
		Name:           "质量",
		Category:       CategoryQuality,
		Scale:          Scale0To100,
		Description:    "高 ROE、稳定盈利、低负债，基本面稳健",
		HigherIsBetter: true,
	},
	"momentum": {
		ID:             "momentum",
		Name:           "动量",
		Category:       CategoryMomentum,
		Scale:          Scale0To100,
		Description:    "中长期走势延续性（1/3/6/12 月收益率综合）",
		HigherIsBetter: true,
	},
	"low_volatility": {
		ID:             "low_volatility",
		Name:           "低波动",
		Category:       CategoryVolatility,
		Scale:          Scale0To100,
		Description:    "历史波动率、振幅越低分数越高",
		HigherIsBetter: true,
	},
	"earnings_stability": {
		ID:             "earnings_stability",
		Name:           "盈利稳定性",
		Category:       CategoryQuality,
		Scale:          Scale0To100,
		Description:    "季度盈利波动率、同比增长一致性",
		HigherIsBetter: true,
	},
	"liquidity": {
		ID:             "liquidity",
		Name:           "流动性",
		Category:       CategoryLiquidity,
		Scale:          Scale0To100,
		Description:    "成交额/换手率/冲击成本",
		HigherIsBetter: true,
	},
	"order_book": {
		ID:             "order_book",
		Name:           "盘口",
		Category:       CategorySentiment,
		Scale:          Scale0To100,
		Description:    "量比、内外盘、封板状态（需 order_book_daily 数据）",
		HigherIsBetter: true,
	},
	"cointegration_spread": {
		ID:             "cointegration_spread",
		Name:           "协整残差",
		Category:       CategorySize,
		Scale:          ScaleZScore,
		Description:    "个股相对沪深300残差 z-score，低超卖则高吸引",
		HigherIsBetter: false, // z-score 低（<-2）意味着相对市场超跌，反而有机会
	},
}

// FactorOrder 因子展示顺序（与 screener 的 factorScoreOrder 保持一致）
var FactorOrder = []string{
	"value",
	"quality",
	"momentum",
	"low_volatility",
	"earnings_stability",
	"liquidity",
	"order_book",
	"cointegration_spread",
}

// Get 返回指定因子的元数据（缺失时返回零值，ok=false）
func Get(id string) (FactorMeta, bool) {
	m, ok := AllFactors[id]
	return m, ok
}

// AllCategories 返回全部分类（去重，引用 engine.go 的 Category* 常量）
func AllCategories() []string {
	seen := make(map[string]bool, len(AllFactors))
	var cats []string
	for _, m := range AllFactors {
		if !seen[m.Category] {
			seen[m.Category] = true
			cats = append(cats, m.Category)
		}
	}
	return cats
}
