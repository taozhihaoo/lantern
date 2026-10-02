package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// flagKind 描述一个 flag 是否带值。
type flagKind int

const (
	// flagBool 为布尔开关(不带值,支持 --x / --x=true)。
	flagBool flagKind = iota
	// flagValue 为带值参数。
	flagValue
)

// parseKnown 按 spec 解析参数:flag 可出现在任意位置(值跟随其后),
// 其余为位置参数。未知 flag 视为用法错误。
func parseKnown(args []string, spec map[string]flagKind) (*parsedValues, []string, bool) {
	pv := &parsedValues{m: map[string][]string{}}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		var val string
		hasVal := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			val = name[eq+1:]
			name = name[:eq]
			hasVal = true
		}
		kind, ok := spec[name]
		if !ok {
			return nil, nil, true
		}
		switch kind {
		case flagBool:
			if hasVal {
				pv.m[name] = append(pv.m[name], val)
			} else {
				pv.m[name] = append(pv.m[name], "true")
			}
		case flagValue:
			if !hasVal {
				if i+1 >= len(args) {
					return nil, nil, true
				}
				i++
				val = args[i]
			}
			pv.m[name] = append(pv.m[name], val)
		}
	}
	return pv, pos, false
}

// parsedValues 提供带默认值的读取。
type parsedValues struct {
	m map[string][]string
}

func (p *parsedValues) last(name string) (string, bool) {
	v := p.m[name]
	if len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

// str 返回字符串值(默认 def)。
func (p *parsedValues) str(name, def string) string {
	if v, ok := p.last(name); ok {
		return v
	}
	return def
}

// boolean 返回布尔值。
func (p *parsedValues) boolean(name string) bool {
	v, ok := p.last(name)
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return true // 裸 --flag 形式
	}
	return b
}

// int 返回整数值。
func (p *parsedValues) int(name string, def int) int {
	v, ok := p.last(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// float 返回浮点值。
func (p *parsedValues) float(name string, def float64) float64 {
	v, ok := p.last(name)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// 各子命令的 flag 规格。
var (
	indexFlags = map[string]flagKind{
		"workers":       flagValue,
		"max-size":      flagValue,
		"no-store-body": flagBool,
	}
	watchFlags = map[string]flagKind{
		"interval":      flagValue,
		"workers":       flagValue,
		"no-store-body": flagBool,
	}
	searchFlags = map[string]flagKind{
		"n":         flagValue,
		"json":      flagBool,
		"explain":   flagBool,
		"no-color":  flagBool,
		"recency":   flagValue,
		"half-life": flagValue,
	}
)

var _ = fmt.Sprintf

var whyNotFlags = map[string]flagKind{
	"json": flagBool,
}

var serveFlags = map[string]flagKind{
	"addr": flagValue,
}
