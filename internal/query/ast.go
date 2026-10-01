package query

import (
	"lantern/internal/index"
)

// Node 是查询 AST 节点。
type Node interface{ node() }

// AndNode 是合取(全部子节点命中)。
type AndNode struct {
	Children []Node
}

// OrNode 是析取(任一子节点命中)。
type OrNode struct {
	Children []Node
}

// NotNode 是排除。
type NotNode struct {
	Child Node
}

// TermNode 是词项(可带字段限定/前缀/模糊修饰)。
type TermNode struct {
	Field    index.Field
	HasField bool
	Text     string
	IsPrefix bool
	// Fuzzy:无修饰时 HasFuzzy=false;自动级 = -1;显式 0..2。
	HasFuzzy bool
	Fuzzy    int
	col      int
}

// PhraseNode 是短语/邻近(基于位置)。
type PhraseNode struct {
	Field    index.Field
	HasField bool
	Raw      string // 引号内原文(规划期分析)
	Slop     int    // 0 = 严格短语;-1 表示非邻近的普通短语
	HasSlop  bool
	col      int
}

// FilterNode 是 ext/size/mtime 过滤。
type FilterNode struct {
	Field string // ext / size / mtime
	Op    string // "=" ">" ">=" "<" "<="
	Val   string // 原始值
	col   int
}

// MatchAllNode 匹配段内全部存活文档(纯否定/纯过滤查询的种子)。
type MatchAllNode struct{}

func (*AndNode) node()      {}
func (*OrNode) node()       {}
func (*NotNode) node()      {}
func (*TermNode) node()     {}
func (*PhraseNode) node()   {}
func (*FilterNode) node()   {}
func (*MatchAllNode) node() {}

// Parser 把词法流组装为 AST。
type Parser struct {
	lx    *Lexer
	toks  []token
	pos   int
	query string
}

// Parse 解析查询串。语法错误返回 *SyntaxError。
func Parse(q string) (Node, error) {
	lx := NewLexer(q)
	toks, err := lx.Lex()
	if err != nil {
		return nil, err
	}
	p := &Parser{lx: lx, toks: toks, query: q}
	if len(toks) == 0 || toks[0].kind == tokEOF {
		return nil, syntaxErr(q, 1, 1, "空查询")
	}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tokEOF {
		if t.kind == tokRParen {
			return nil, syntaxErr(q, 1, t.col, "多余的右括号")
		}
		return nil, syntaxErr(q, 1, t.col, "意外词法单元 %q", t.raw)
	}
	return n, nil
}

func (p *Parser) peek() token { return p.toks[p.pos] }
func (p *Parser) next() token { t := p.toks[p.pos]; p.pos++; return t }

// parseOr: and (OR and)*。
func (p *Parser) parseOr() (Node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	kids := []Node{left}
	for {
		t := p.peek()
		if t.kind != tokOr {
			break
		}
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		kids = append(kids, right)
	}
	if len(kids) == 1 {
		return kids[0], nil
	}
	return &OrNode{Children: kids}, nil
}

// parseAnd: not ((AND)? not)*,词之间隐式 AND。
func (p *Parser) parseAnd() (Node, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	kids := []Node{left}
	for {
		t := p.peek()
		if t.kind == tokAnd {
			p.next()
			t = p.peek()
		}
		switch t.kind {
		case tokTerm, tokPhrase, tokLParen, tokMinus, tokNot:
			kid, err := p.parseNot()
			if err != nil {
				return nil, err
			}
			kids = append(kids, kid)
		default:
			goto done
		}
	}
done:
	if len(kids) == 1 {
		return kids[0], nil
	}
	return &AndNode{Children: kids}, nil
}

// parseNot: (NOT|'-') not | atom。
func (p *Parser) parseNot() (Node, error) {
	t := p.peek()
	if t.kind == tokNot || t.kind == tokMinus {
		p.next()
		child, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &NotNode{Child: child}, nil
	}
	return p.parseAtom()
}

// parseAtom: '(' or ')' | phrase | term。
func (p *Parser) parseAtom() (Node, error) {
	t := p.peek()
	switch t.kind {
	case tokLParen:
		p.next()
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if c := p.peek(); c.kind != tokRParen {
			return nil, syntaxErr(p.query, 1, t.col, "缺少右括号")
		}
		p.next()
		return inner, nil
	case tokPhrase:
		p.next()
		return &PhraseNode{
			Field:    fieldOrDefault(t.field),
			HasField: t.field != "",
			Raw:      t.text,
			Slop:     t.slop,
			HasSlop:  t.slopSet,
			col:      t.col,
		}, nil
	case tokTerm:
		p.next()
		return p.buildTerm(t)
	case tokEOF:
		return nil, syntaxErr(p.query, 1, t.col, "查询意外结束")
	case tokRParen:
		return nil, syntaxErr(p.query, 1, t.col, "意外的右括号")
	case tokAnd, tokOr:
		return nil, syntaxErr(p.query, 1, t.col, "%s 缺少操作数", t.raw)
	}
	return nil, syntaxErr(p.query, 1, t.col, "意外的词法单元")
}

func fieldOrDefault(name string) index.Field {
	if f, ok := index.FieldByName(name); ok {
		return f
	}
	return index.NumFields
}

// buildTerm 把词项 token 转为 TermNode/FilterNode;过滤字段的值解析
// 失败在规划期报告。
func (p *Parser) buildTerm(t token) (Node, error) {
	switch t.field {
	case "ext", "size", "mtime":
		f := &FilterNode{Field: t.field, col: t.col}
		val := t.text
		for _, op := range []string{">=", "<=", ">", "<", "="} {
			if len(val) > len(op) && val[:len(op)] == op {
				f.Op = op
				f.Val = val[len(op):]
				goto parsed
			}
		}
		f.Op = "="
		f.Val = val
	parsed:
		if f.Val == "" {
			return nil, syntaxErr(p.query, 1, t.col, "%s 过滤缺少值", t.field)
		}
		if t.field != "ext" {
			if err := validateFilterValue(t.field, f.Val); err != nil {
				return nil, syntaxErr(p.query, 1, t.col, "%s", err.Error())
			}
		}
		return f, nil
	}
	return &TermNode{
		Field:    fieldOrDefault(t.field),
		HasField: t.field != "",
		Text:     t.text,
		IsPrefix: t.isPrefix,
		HasFuzzy: t.hasFuzzy,
		Fuzzy:    t.fuzzy,
		col:      t.col,
	}, nil
}
