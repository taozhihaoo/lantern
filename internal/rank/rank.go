// Package rank 实现 BM25F 排序、recency 加成与 Explain 结构。
//
// 公式(规格 7.1):
//
//	tfw(t,d)   = Σ_f  w_f · tf_f(t,d) / (1 - b_f + b_f · len_f(d)/avglen_f)
//	score(t,d) = idf(t) · tfw / (k1 + tfw)
//	idf(t)     = ln(1 + (N - df + 0.5)/(df + 0.5))
//
// df(t) 取各字段 df 的最大值(文档化的近似);N 与 df 统计包含
// 已删除但未合并的文档(与 Lucene 一致)。
package rank

import (
	"fmt"
	"math"
	"strings"

	"lantern/internal/index"
)

// Params 是 BM25F 参数,可由配置文件覆盖。
type Params struct {
	K1 float64                  `json:"k1"`
	W  [index.NumFields]float64 `json:"w"`
	B  [index.NumFields]float64 `json:"b"`
}

// DefaultParams 返回规格默认参数:
// k1=1.2;w_title=3.0, w_path=2.0, w_body=1.0;b_title=0.5, b_path=0.5, b_body=0.75。
func DefaultParams() Params {
	return Params{
		K1: 1.2,
		W:  [index.NumFields]float64{index.FieldPath: 2.0, index.FieldTitle: 3.0, index.FieldBody: 1.0},
		B:  [index.NumFields]float64{0.5, 0.5, 0.75},
	}
}

// FieldStats 是跨段汇总后的全局统计。
type FieldStats struct {
	// TotalLen 为该字段 token 总数(跨段求和)。
	TotalLen float64
	// DocCount 为含该字段至少一个 token 的文档数(跨段求和)。
	DocCount float64
}

// AvgLen 返回平均字段长度;无文档时返回 0。
func (fs FieldStats) AvgLen() float64 {
	if fs.DocCount <= 0 {
		return 0
	}
	return fs.TotalLen / fs.DocCount
}

// Stats 是打分所需的全局统计(N、df、avglen)。
type Stats struct {
	// N 为全部文档数(含已删除未合并)。
	N uint64
	// Fields 为各字段的全局长度统计。
	Fields [index.NumFields]FieldStats
}

// Idf 计算 idf(t) = ln(1 + (N - df + 0.5)/(df + 0.5))。
func Idf(n, df uint64) float64 {
	if df == 0 {
		return 0
	}
	return math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
}

// TFW 计算 tfw(t,d)。tf 为各字段词频,len 为各字段长度,
// avg 为各字段平均长度。
func TFW(p Params, tf, length, avg [index.NumFields]float64) float64 {
	sum := 0.0
	for f := 0; f < int(index.NumFields); f++ {
		if tf[f] <= 0 {
			continue
		}
		denom := 1 - p.B[f] + p.B[f]
		if avg[f] > 0 {
			denom = 1 - p.B[f] + p.B[f]*length[f]/avg[f]
		}
		if denom <= 0 {
			denom = 1e-9
		}
		sum += p.W[f] * tf[f] / denom
	}
	return sum
}

// TermScore 计算单个词项对文档的得分贡献:
// weight · idf · tfw / (k1 + tfw)。weight 用于模糊/前缀展开
// (1 - dist/len),普通词项为 1。
func TermScore(p Params, idf, tfw, weight float64) float64 {
	if tfw <= 0 {
		return 0
	}
	return weight * idf * tfw / (p.K1 + tfw)
}

// RecencyBoost 计算 recency 加成因子:
// 1 + r · 0.5^(ageDays/halfLifeDays)。r=0(默认)时为 1(关闭)。
func RecencyBoost(r, halfLifeDays, ageDays float64) float64 {
	if r <= 0 || halfLifeDays <= 0 {
		return 1
	}
	return 1 + r*math.Pow(0.5, ageDays/halfLifeDays)
}

// FieldExplain 是单个词项在单个字段上的拆解。
type FieldExplain struct {
	Field  string  `json:"field"`
	TF     float64 `json:"tf"`
	Len    float64 `json:"len"`
	AvgLen float64 `json:"avglen"`
	W      float64 `json:"w"`
	B      float64 `json:"b"`
	TFW    float64 `json:"tfw"`
}

// TermExplain 是单个查询词项的贡献拆解。
type TermExplain struct {
	Text string `json:"text"`
	// Weight 为展开权重(模糊词 = 1 - dist/len,普通词 = 1)。
	Weight float64 `json:"weight"`
	DF     uint64  `json:"df"`
	IDF    float64 `json:"idf"`
	// DFIsMax 表明 df 取的是各字段 df 的最大值(文档化近似)。
	DFIsMax bool           `json:"df_is_max"`
	Fields  []FieldExplain `json:"fields"`
	// Contribution = Weight · IDF · TFW_total/(k1+TFW_total)。
	Contribution float64 `json:"contribution"`
}

// Explain 是一条结果的完整得分拆解,可 JSON 序列化。
type Explain struct {
	N     uint64        `json:"n"`
	Terms []TermExplain `json:"terms"`
	// Boost 为 recency 乘法因子(1 表示关闭)。
	Boost float64 `json:"boost"`
	// FinalScore = Σ Contribution(按 Terms 顺序求和)· Boost。
	FinalScore float64 `json:"final_score"`
	// Truncated 表示前缀/单字展开超出上限。
	Truncated bool `json:"truncated"`
}

// Sum 返回各词项贡献之和(按存储顺序)。
func (e *Explain) Sum() float64 {
	s := 0.0
	for i := range e.Terms {
		s += e.Terms[i].Contribution
	}
	return s
}

// RenderText 输出缩进文本树。
func (e *Explain) RenderText() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "score = %.6f  (N=%d, boost=%.4f)\n", e.FinalScore, e.N, e.Boost)
	if e.Truncated {
		sb.WriteString("  [展开超出上限,结果被截断标记]\n")
	}
	for i := range e.Terms {
		t := &e.Terms[i]
		fmt.Fprintf(&sb, "  term %q weight=%.3f df=%d idf=%.4f contrib=%.6f\n",
			t.Text, t.Weight, t.DF, t.IDF, t.Contribution)
		for j := range t.Fields {
			f := &t.Fields[j]
			fmt.Fprintf(&sb, "    %s: tf=%g len=%g avglen=%.4f w=%.2f b=%.2f tfw=%.6f\n",
				f.Field, f.TF, f.Len, f.AvgLen, f.W, f.B, f.TFW)
		}
	}
	return sb.String()
}
