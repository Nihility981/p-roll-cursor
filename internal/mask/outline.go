package mask

import (
	"fmt"
	"sort"
	"strings"
)

// Outline 生成 JSON 的结构概览（key、类型与值），用于确认真实响应结构。
// maxDepth 控制展开层数；过长的字符串只在概览里截断以保持可读，
// 完整值仍会原样写进落盘的原始响应文件。
func Outline(v any, maxDepth int) []string {
	var lines []string
	outline(&lines, "", v, 0, maxDepth)
	return lines
}

func outline(lines *[]string, prefix string, v any, depth, maxDepth int) {
	indent := strings.Repeat("  ", depth)
	switch t := v.(type) {
	case map[string]any:
		if depth >= maxDepth {
			*lines = append(*lines, fmt.Sprintf("%s%s: object(%d keys, 已折叠)", indent, prefix, len(t)))
			return
		}
		if prefix != "" {
			*lines = append(*lines, fmt.Sprintf("%s%s: object(%d keys)", indent, prefix, len(t)))
			depth++
			indent = strings.Repeat("  ", depth)
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			outline(lines, k, t[k], depth, maxDepth)
		}
	case []any:
		*lines = append(*lines, fmt.Sprintf("%s%s: array(len=%d)", indent, prefix, len(t)))
		if len(t) > 0 && depth < maxDepth {
			outline(lines, "[0]", t[0], depth+1, maxDepth)
		}
	case nil:
		*lines = append(*lines, fmt.Sprintf("%s%s: null", indent, prefix))
	case bool:
		*lines = append(*lines, fmt.Sprintf("%s%s: bool = %v", indent, prefix, t))
	case float64:
		*lines = append(*lines, fmt.Sprintf("%s%s: number = %v", indent, prefix, t))
	case string:
		*lines = append(*lines, fmt.Sprintf("%s%s: string = %q", indent, prefix, truncate(t, 48)))
	default:
		*lines = append(*lines, fmt.Sprintf("%s%s: %T", indent, prefix, t))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
