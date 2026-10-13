package sixdim

// 本文件提供跨模块统一的「市场环境」口径。
//
// 背景：项目里曾并存四套 market regime 口径——六维判势的
// 「强势/结构性震荡/偏弱/退潮风险」、cio 的 BULLISH/NEUTRAL/BEARISH、
// screener 的 bull/bull_range/range/bear/bear_range——各处独立判断、
// 结论可能互相打架，导致「弱市现金保护」在不同链路生效不一致。
//
// SimpleRegime 把上述所有口径归一到三态，并以六维判势（市场六维判势
// 是盘前决策与盘中监控的统一依据）作为唯一裁决来源：其它模块需要判断
// 市场强弱时，一律调用 ToSimpleRegime，不再各自实现。

// SimpleRegime 统一市场环境三态
type SimpleRegime string

const (
	// SimpleBullish 多头环境：可正常建仓/加仓
	SimpleBullish SimpleRegime = "BULLISH"
	// SimpleNeutral 震荡环境：允许结构性机会，控制仓位
	SimpleNeutral SimpleRegime = "NEUTRAL"
	// SimpleBearish 弱势环境：只降不升，主动降低权益敞口
	SimpleBearish SimpleRegime = "BEARISH"
)

// ToSimpleRegime 基于六维判势的市场标签与仓位系数，归一为统一的三态市场环境。
//
// 判定口径（与六维策略.md 的「分数-仓位-标签」映射严格对应）：
//   - 标签「退潮风险」            → BEARISH
//   - 标签「偏弱」且仓位系数≤0.3  → BEARISH，否则 NEUTRAL
//   - 标签「强势」                → BULLISH
//   - 标签缺失/未知时按仓位系数阈值：≤0.2 → BEARISH；≥0.6 → BULLISH；其余 NEUTRAL
//
// 标签与仓位系数冲突时以更保守者为准：先判显式弱标签，再判显式强标签，
// 最后才用仓位系数兜底，避免「高仓位标签」掩盖「退潮风险」。
func ToSimpleRegime(tag string, positionRate float64) SimpleRegime {
	switch tag {
	case "退潮风险":
		return SimpleBearish
	case "偏弱":
		if positionRate <= 0.3 {
			return SimpleBearish
		}
		return SimpleNeutral
	case "强势":
		return SimpleBullish
	}
	if positionRate <= 0.2 {
		return SimpleBearish
	}
	if positionRate >= 0.6 {
		return SimpleBullish
	}
	return SimpleNeutral
}