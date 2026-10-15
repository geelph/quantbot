// Package data benchmark_resolver.go — 按标的市场前缀自动选择 benchmark。
//
// 对应 TradingAgents 的 _resolve_benchmark：
//   benchmark_map = { ".HK": "^HSI", ".T": "^N225", ".L": "^FTSE", ".NS": "^NSEI", "": "SPY" }
//
// QuantBot 全 A 股，按 symbol 前缀映射：
//   sh + 000/300 → 沪深300
//   sh + 688     → 科创50
//   sh + 600     → 上证50（按行业可细分成沪深300）
//   sz + 300     → 创业板指
//   sz + 000/001 → 深证100（沪深300兜底）
//   bj           → 北证50
//   index codes  → 直接用自身（sh000300 benchmark 就是 sh000300）
//
// 同时暴露 IndexList（全 A 核心指数代码），供六维判势/因子选股/回测基准使用。
package data

import (
	"strings"
)

// BenchmarkInfo benchmark 信息
type BenchmarkInfo struct {
	Symbol     string // 指数代码（DuckDB 中的 symbol）
	Name       string // 中文名
	Desc       string // 描述
}

// ResolveBenchmark 按 symbol 前缀自动选 benchmark。
// 返回 DuckDB 中的 symbol（如 sh000300）。无法识别时返回 DefaultAStockBenchmark。
func ResolveBenchmark(symbol string) BenchmarkInfo {
	if symbol == "" {
		return DefaultAStockBenchmark
	}
	s := strings.ToLower(strings.TrimSpace(symbol))

	// 指数代码 → 直接用自身当 benchmark
	if IsIndexCode(s) {
		info := IndexInfo(s)
		return BenchmarkInfo{Symbol: s, Name: info.Name, Desc: info.Desc}
	}

	switch {
	case strings.HasPrefix(s, "bj"):
		return BenchmarkBJ50
	case strings.HasPrefix(s, "sh688"):
		return BenchmarkSTAR50
	case strings.HasPrefix(s, "sz300"):
		return BenchmarkChiNext
	case strings.HasPrefix(s, "sh"):
		return BenchmarkHS300
	case strings.HasPrefix(s, "sz"):
		return BenchmarkSZ100
	default:
		return DefaultAStockBenchmark
	}
}

// IsIndexCode 判断是否指数代码（A 股：000001/399006/899050 + sh/sz/bj 前缀）
func IsIndexCode(s string) bool {
	s = strings.ToLower(s)
	// 常见指数代码：sh000300 / sz399006 / bj899050 / sh000016 等
	suffix := s
	for _, prefix := range []string{"sh", "sz", "bj"} {
		if strings.HasPrefix(s, prefix) {
			suffix = strings.TrimPrefix(s, prefix)
			break
		}
	}
	// 指数代码以 0 或 3 或 8 开头（000xxx/001xxx/399xxx/899xxx）
	if len(suffix) != 6 {
		return false
	}
	first := suffix[0]
	return first == '0' || first == '3' || first == '8'
}

// IndexInfo 返回指定指数的元数据（不在映射里返回空）
func IndexInfo(symbol string) BenchmarkInfo {
	if v, ok := benchmarkMap[strings.ToLower(symbol)]; ok {
		return v
	}
	return BenchmarkInfo{}
}

// === 预置 benchmark 常量 ===

var (
	DefaultAStockBenchmark = BenchmarkHS300
	BenchmarkHS300         = BenchmarkInfo{Symbol: "sh000300", Name: "沪深300", Desc: "大盘价值+成长均衡基准"}
	BenchmarkSZ100         = BenchmarkInfo{Symbol: "sz399013", Name: "深证100", Desc: "深市大盘基准"}
	BenchmarkSTAR50        = BenchmarkInfo{Symbol: "sh000016", Name: "科创50", Desc: "科创板基准"}
	BenchmarkChiNext       = BenchmarkInfo{Symbol: "sz399006", Name: "创业板指", Desc: "创业板基准"}
	BenchmarkBJ50          = BenchmarkInfo{Symbol: "bj899050", Name: "北证50", Desc: "北交所基准"}
	BenchmarkCSI500        = BenchmarkInfo{Symbol: "sh000905", Name: "中证500", Desc: "中盘基准"}
	BenchmarkCSI1000       = BenchmarkInfo{Symbol: "sh000852", Name: "中证1000", Desc: "小盘基准"}
	BenchmarkSH50          = BenchmarkInfo{Symbol: "sh000016", Name: "上证50", Desc: "超大盘基准"}
)

// benchmarkMap 指数代码 → 元数据（用于 IsIndexCode 已知判断 + IndexInfo 查询）
var benchmarkMap = map[string]BenchmarkInfo{
	"sh000300": BenchmarkHS300,
	"sz399013": BenchmarkSZ100,
	"sh000016": BenchmarkSTAR50, // 注意：sh000016 是上证50，同时被 ResolveBenchmark 用来映射 sh688
	"sz399006": BenchmarkChiNext,
	"bj899050": BenchmarkBJ50,
	"sh000905": BenchmarkCSI500,
	"sh000852": BenchmarkCSI1000,
}

// AllBenchmarks 返回全 A 核心指数列表（供六维判势/因子选股/回测基准使用）
func AllBenchmarks() []BenchmarkInfo {
	return []BenchmarkInfo{
		BenchmarkHS300, BenchmarkSZ100, BenchmarkSH50,
		BenchmarkSTAR50, BenchmarkChiNext, BenchmarkBJ50,
		BenchmarkCSI500, BenchmarkCSI1000,
	}
}
