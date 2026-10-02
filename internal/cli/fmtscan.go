package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// fmtSscan 解析十进制浮点(避免直接依赖 fmt.Sscanf 的格式歧义)。
func fmtSscan(s string, v *float64) (int, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	*v = f
	return 1, nil
}

var _ = fmt.Sprintf
