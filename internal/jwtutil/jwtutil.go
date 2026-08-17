// Package jwtutil 用标准库解析 JWT payload。
// 这里只做「解码读取」，不做签名校验——token 是本机 Cursor 自己写下的，
// 我们只需要读出 sub / exp，没有引入第三方 JWT 库的必要。
package jwtutil

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Payload 是我们关心的 JWT 载荷字段。
type Payload struct {
	Sub    string         `json:"sub"`
	Exp    int64          `json:"exp"`
	Iat    int64          `json:"iat"`
	Iss    string         `json:"iss"`
	Aud    any            `json:"aud"`
	Scope  string         `json:"scope"`
	Raw    map[string]any `json:"-"`
	RawB64 string         `json:"-"`
}

// ExpiresAt 返回过期时间；exp 缺失时返回零值。
func (p *Payload) ExpiresAt() time.Time {
	if p.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(p.Exp, 0)
}

// TimeToExpiry 返回距离过期还有多久，可能为负（已过期）。
func (p *Payload) TimeToExpiry() time.Duration {
	if p.Exp == 0 {
		return 0
	}
	return time.Until(p.ExpiresAt())
}

// Decode 解析 JWT 的第二段（payload）。
func Decode(token string) (*Payload, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("JWT 为空")
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("JWT 格式不正确：期望至少 2 段，实际 %d 段", len(parts))
	}

	decoded, err := decodeSegment(parts[1])
	if err != nil {
		return nil, fmt.Errorf("Base64URL 解码 JWT payload 失败: %w", err)
	}

	var payload Payload
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil, fmt.Errorf("解析 JWT payload JSON 失败: %w", err)
	}
	raw := map[string]any{}
	if err := json.Unmarshal(decoded, &raw); err == nil {
		payload.Raw = raw
	}
	payload.RawB64 = parts[1]
	return &payload, nil
}

// decodeSegment 处理 Base64URL 的 padding 缺失问题。
// 标准 JWT 去掉了 '=' 补位，RawURLEncoding 正好对应；
// 但个别实现会保留 padding，这里两种都兼容。
func decodeSegment(seg string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(seg); err == nil {
		return b, nil
	}
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(seg)
}

// WorkosUserIDFromSub 复刻 Rust 参照实现的提取逻辑：
// sub 按 '|' 切取最后一段，且必须以 "user_" 开头，否则视为不可用。
func WorkosUserIDFromSub(sub string) (string, bool) {
	if sub == "" {
		return "", false
	}
	last := sub
	if i := strings.LastIndex(sub, "|"); i >= 0 {
		last = sub[i+1:]
	}
	if strings.HasPrefix(last, "user_") {
		return last, true
	}
	return last, false
}
