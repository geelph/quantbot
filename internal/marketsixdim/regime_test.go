package sixdim

import "testing"

// TestToSimpleRegime 覆盖统一市场环境归一的全部规则分支：
// 显式弱标签优先、显式强标签、以及标签缺失时按仓位系数兜底。
func TestToSimpleRegime(t *testing.T) {
	cases := []struct {
		name string
		tag  string
		rate float64
		want SimpleRegime
	}{
		{"退潮风险恒为熊市", "退潮风险", 0.9, SimpleBearish},
		{"退潮风险不被高仓位掩盖", "退潮风险", 1.0, SimpleBearish},
		{"偏弱且仓位恰好0.3为熊市", "偏弱", 0.3, SimpleBearish},
		{"偏弱且低仓位为熊市", "偏弱", 0.1, SimpleBearish},
		{"偏弱但仓位不低为震荡", "偏弱", 0.31, SimpleNeutral},
		{"偏弱高仓位不越级为牛市", "偏弱", 0.9, SimpleNeutral},
		{"强势为牛市", "强势", 0.1, SimpleBullish},
		{"未知标签低仓位为熊市", "结构性震荡", 0.2, SimpleBearish},
		{"未知标签高仓位为牛市", "结构性震荡", 0.6, SimpleBullish},
		{"未知标签中仓位为震荡", "结构性震荡", 0.4, SimpleNeutral},
		{"空标签中仓位为震荡", "", 0.5, SimpleNeutral},
	}
	for _, c := range cases {
		if got := ToSimpleRegime(c.tag, c.rate); got != c.want {
			t.Errorf("%s: ToSimpleRegime(%q, %.2f) = %s, want %s", c.name, c.tag, c.rate, got, c.want)
		}
	}
}