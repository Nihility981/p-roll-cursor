// Package mask 提供统一的取值展示工具。
//
// 早期版本会遮蔽 token / 邮箱 / 用户 ID，现已取消：这是纯本地的个人工具，
// 查的是本机自己的账号，遮蔽只会妨碍排查（例如看不出 sub 到底是不是
// user_ 开头）。这些函数保留下来只做统一的展示格式，值一律原样输出。
package mask

import (
	"fmt"
	"strings"
)

// Token 输出完整 token，并附带长度便于核对。
func Token(s string) string {
	if s == "" {
		return "<空>"
	}
	return fmt.Sprintf("%s(len=%d)", s, len(s))
}

// Email 输出完整邮箱。
func Email(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "<空>"
	}
	return s
}

// ID 输出完整标识符（auth0|xxx / user_xxx 这类），并附带长度。
func ID(s string) string {
	if s == "" {
		return "<空>"
	}
	return fmt.Sprintf("%s(len=%d)", s, len(s))
}

// Shape 描述一个字符串的「形态」：把字母数字替换为 x/X/9。
// 用于一眼看清 sub 这类标识符的构成，与遮蔽无关。
func Shape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune('9')
		case r >= 'a' && r <= 'z':
			b.WriteRune('x')
		case r >= 'A' && r <= 'Z':
			b.WriteRune('X')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
