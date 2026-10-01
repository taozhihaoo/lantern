package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"lantern/internal/index"
)

// parseSize 解析 "1mb"/"10kb"/"512" 为字节数(二进制单位)。
func parseSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, fmt.Errorf("size 值为空")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "kb"):
		mult, s = 1<<10, s[:len(s)-2]
	case strings.HasSuffix(s, "mb"):
		mult, s = 1<<20, s[:len(s)-2]
	case strings.HasSuffix(s, "gb"):
		mult, s = 1<<30, s[:len(s)-2]
	case strings.HasSuffix(s, "b"):
		mult, s = 1, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("非法 size 值 %q", s)
	}
	return n * mult, nil
}

// parseTimeValue 解析 mtime 过滤值,返回 [起,止) Unix 秒区间:
// "2025-01-01" → 当天;"2025-06" → 当月;支持 "2025"。
func parseTimeValue(s string) (start, end int64, err error) {
	s = strings.TrimSpace(s)
	loc := time.UTC
	if len(s) == 4 && allDigits(s) {
		y, _ := strconv.Atoi(s)
		t := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
		return t.Unix(), t.AddDate(1, 0, 0).Unix(), nil
	}
	if len(s) == 7 && s[4] == '-' {
		y, e1 := strconv.Atoi(s[:4])
		m, e2 := strconv.Atoi(s[5:7])
		if e1 != nil || e2 != nil || m < 1 || m > 12 {
			return 0, 0, fmt.Errorf("非法 mtime 月份 %q", s)
		}
		t := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, loc)
		return t.Unix(), t.AddDate(0, 1, 0).Unix(), nil
	}
	if len(s) == 10 && s[4] == '-' && s[7] == '-' {
		y, e1 := strconv.Atoi(s[:4])
		m, e2 := strconv.Atoi(s[5:7])
		d, e3 := strconv.Atoi(s[8:10])
		if e1 != nil || e2 != nil || e3 != nil || m < 1 || m > 12 || d < 1 || d > 31 {
			return 0, 0, fmt.Errorf("非法 mtime 日期 %q", s)
		}
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
		return t.Unix(), t.AddDate(0, 0, 1).Unix(), nil
	}
	return 0, 0, fmt.Errorf("非法 mtime 值 %q(支持 YYYY / YYYY-MM / YYYY-MM-DD)", s)
}

// validateFilterValue 检查过滤值语法(解析期)。
func validateFilterValue(field, val string) error {
	switch field {
	case "size":
		_, err := parseSize(val)
		return err
	case "mtime":
		_, _, err := parseTimeValue(val)
		return err
	}
	return nil
}

// filterCond 是编译后的过滤条件,基于段内 doc values 判定。
type filterCond struct {
	field        string
	op           string
	sizeN        int64
	tStart, tEnd int64
	ext          string
}

// compileFilter 把 FilterNode 编译为 filterCond。
func compileFilter(n *FilterNode) (*filterCond, error) {
	fc := &filterCond{field: n.Field, op: n.Op}
	switch n.Field {
	case "ext":
		fc.ext = strings.TrimPrefix(strings.ToLower(n.Val), ".")
		if fc.ext == "" {
			return nil, fmt.Errorf("ext 过滤值不能为空")
		}
	case "size":
		n64, err := parseSize(n.Val)
		if err != nil {
			return nil, err
		}
		fc.sizeN = n64
	case "mtime":
		st, en, err := parseTimeValue(n.Val)
		if err != nil {
			return nil, err
		}
		fc.tStart, fc.tEnd = st, en
	default:
		return nil, fmt.Errorf("未知过滤字段 %q", n.Field)
	}
	return fc, nil
}

// match 对段内某文档求值。
func (fc *filterCond) match(r *index.SegmentReader, docID uint32) bool {
	dv, err := r.DocValues(docID)
	if err != nil {
		return false
	}
	switch fc.field {
	case "ext":
		return dv.Ext == fc.ext
	case "size":
		return compareInt(dv.Size, fc.op, fc.sizeN)
	case "mtime":
		if fc.op == "=" {
			return dv.MTime >= fc.tStart && dv.MTime < fc.tEnd
		}
		th := fc.tStart
		if fc.op == "<" || fc.op == "<=" {
			th = fc.tEnd - 1
		}
		return compareInt(dv.MTime, fc.op, th)
	}
	return false
}

func compareInt(v int64, op string, t int64) bool {
	switch op {
	case ">":
		return v > t
	case ">=":
		return v >= t
	case "<":
		return v < t
	case "<=":
		return v <= t
	case "=", "":
		return v == t
	}
	return false
}

// describe 输出条件的可读形式(Why-not 用)。
func (fc *filterCond) describe() string {
	switch fc.field {
	case "ext":
		return fmt.Sprintf("ext %s %s", fc.op, fc.ext)
	case "size":
		return fmt.Sprintf("size %s %d 字节", fc.op, fc.sizeN)
	case "mtime":
		return fmt.Sprintf("mtime %s %s(区间 [%d, %d))", fc.op, fc.describeTime(), fc.tStart, fc.tEnd)
	}
	return fc.field
}

func (fc *filterCond) describeTime() string {
	return time.Unix(fc.tStart, 0).UTC().Format("2006-01-02T15:04:05Z")
}
