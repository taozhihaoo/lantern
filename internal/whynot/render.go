package whynot

import (
	"encoding/json"
	"fmt"
	"strings"

	"lantern/internal/index"
	"lantern/internal/query"
)

// RenderText 输出确定性的文本报告(golden 测试与 CLI 共用)。
func (r *Report) RenderText() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "why-not: query=%q path=%s\n", r.Query, r.Path)

	if !r.InIndex {
		sb.WriteString("文档层: 该文档不在索引中\n")
		if r.Document != nil && r.Document.Reason != "" {
			fmt.Fprintf(&sb, "  原因: %s — %s\n", r.Document.Reason, r.Document.Detail)
			if r.Document.Rule != nil {
				fmt.Fprintf(&sb, "  规则: %s (%s:%d)\n",
					r.Document.Rule.Pattern, r.Document.Rule.Source, r.Document.Rule.Line)
			}
		}
		return sb.String()
	}

	if r.Match {
		sb.WriteString("结论: 该文档命中查询\n")
		return sb.String()
	}

	if r.Document != nil && r.Document.Reason == "stale" {
		sb.WriteString("文档层: 索引已过期\n")
		fmt.Fprintf(&sb, "  %s\n", r.Document.Detail)
		if r.Document.Indexed != nil {
			fmt.Fprintf(&sb, "  索引记录: size=%d mtime=%d sha=%s\n",
				r.Document.Indexed.Size, r.Document.Indexed.MTime, r.Document.Indexed.SHA)
		}
		if r.Document.Disk != nil {
			fmt.Fprintf(&sb, "  磁盘现状: size=%d mtime=%d sha=%s\n",
				r.Document.Disk.Size, r.Document.Disk.MTime, r.Document.Disk.SHA)
		}
	}

	if len(r.Clauses) > 0 {
		sb.WriteString("子句层:\n")
		for _, c := range r.Clauses {
			fmt.Fprintf(&sb, "  [%s] %s — %s\n", c.Status, c.Clause, c.Detail)
			for _, p := range c.Positions {
				fmt.Fprintf(&sb, "    位置: %s@%s = %v\n", p.Term, p.Field, p.Pos)
			}
			if c.Actual != "" || c.Required != "" {
				fmt.Fprintf(&sb, "    实际: %s / 要求: %s\n", c.Actual, c.Required)
			}
			for _, n := range c.Nearest {
				fmt.Fprintf(&sb, "    最近词: %s (距离 %d, %s@%d)\n", n.Term, n.Dist, n.Field, n.Pos)
			}
		}
	}

	if len(r.Suggestions) > 0 {
		sb.WriteString("建议层:\n")
		for _, s := range r.Suggestions {
			if s.EstimatedRank > 0 {
				fmt.Fprintf(&sb, "  移除子句 %q 后可命中:新查询 %q,估计排名 %d/%d\n",
					s.RemoveClause, s.NewQuery, s.EstimatedRank, s.Total)
			} else {
				fmt.Fprintf(&sb, "  %s\n", s.RemoveClause)
			}
		}
	}
	return sb.String()
}

// RenderJSON 输出 JSON 报告。
func (r *Report) RenderJSON() (string, error) {
	b, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return "", fmt.Errorf("whynot: marshal: %w", err)
	}
	return string(b), nil
}

var _ = index.NumFields
var _ = query.Render
