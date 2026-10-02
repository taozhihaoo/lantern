package query

import (
	"strings"

	"lantern/internal/index"
)

// Render 把 AST 还原为可重新解析的查询串(建议层用)。
func Render(n Node) string {
	var sb strings.Builder
	renderNode(&sb, n, false)
	return sb.String()
}

func renderNode(sb *strings.Builder, n Node, nested bool) {
	switch t := n.(type) {
	case *TermNode:
		if t.HasField {
			sb.WriteString(fieldNameOf(t.Field))
			sb.WriteByte(':')
		}
		text := t.Text
		if strings.ContainsAny(text, " \t\"()") {
			text = "\"" + strings.ReplaceAll(text, "\"", "\\\"") + "\""
		}
		sb.WriteString(text)
		if t.IsPrefix {
			sb.WriteByte('*')
		}
		if t.HasFuzzy {
			sb.WriteByte('~')
			if t.Fuzzy >= 0 {
				sb.WriteString(itoa(t.Fuzzy))
			}
		}
	case *PhraseNode:
		if t.HasField {
			sb.WriteString(fieldNameOf(t.Field))
			sb.WriteByte(':')
		}
		sb.WriteByte('"')
		sb.WriteString(strings.ReplaceAll(t.Raw, "\"", "\\\""))
		sb.WriteByte('"')
		if t.HasSlop {
			sb.WriteByte('~')
			sb.WriteString(itoa(t.Slop))
		}
	case *FilterNode:
		sb.WriteString(t.Field)
		sb.WriteByte(':')
		sb.WriteString(t.Op)
		sb.WriteString(t.Val)
	case *NotNode:
		sb.WriteString("-(")
		renderNode(sb, t.Child, false)
		sb.WriteByte(')')
	case *AndNode:
		if nested && len(t.Children) > 1 {
			sb.WriteByte('(')
		}
		for i, c := range t.Children {
			if i > 0 {
				sb.WriteByte(' ')
			}
			renderNode(sb, c, true)
		}
		if nested && len(t.Children) > 1 {
			sb.WriteByte(')')
		}
	case *OrNode:
		if nested {
			sb.WriteByte('(')
		}
		for i, c := range t.Children {
			if i > 0 {
				sb.WriteString(" OR ")
			}
			renderNode(sb, c, true)
		}
		if nested {
			sb.WriteByte(')')
		}
	case *MatchAllNode:
		sb.WriteString("*")
	}
}

func fieldNameOf(f index.Field) string {
	if f == index.NumFields {
		return ""
	}
	return f.Name()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
