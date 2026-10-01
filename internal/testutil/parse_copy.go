package testutil

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseSizeB 与引擎 parseSize 同语义(独立副本,防实现共享掩盖分歧)。
func parseSizeB(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, fmt.Errorf("empty")
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
		return 0, fmt.Errorf("bad size")
	}
	return n * mult, nil
}

// parseTimeB 与引擎 parseTimeValue 同语义。
func parseTimeB(s string) (int64, int64, error) {
	s = strings.TrimSpace(s)
	loc := time.UTC
	if len(s) == 4 {
		y, err := strconv.Atoi(s)
		if err != nil {
			return 0, 0, err
		}
		t := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
		return t.Unix(), t.AddDate(1, 0, 0).Unix(), nil
	}
	if len(s) == 7 {
		y, _ := strconv.Atoi(s[:4])
		m, err := strconv.Atoi(s[5:7])
		if err != nil || m < 1 || m > 12 {
			return 0, 0, fmt.Errorf("bad month")
		}
		t := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, loc)
		return t.Unix(), t.AddDate(0, 1, 0).Unix(), nil
	}
	if len(s) == 10 {
		y, _ := strconv.Atoi(s[:4])
		m, _ := strconv.Atoi(s[5:7])
		d, err := strconv.Atoi(s[8:10])
		if err != nil || m < 1 || m > 12 || d < 1 || d > 31 {
			return 0, 0, fmt.Errorf("bad date")
		}
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
		return t.Unix(), t.AddDate(0, 0, 1).Unix(), nil
	}
	return 0, 0, fmt.Errorf("bad time")
}
