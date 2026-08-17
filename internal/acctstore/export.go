package acctstore

import (
	"fmt"
	"strings"
	"time"

	"github.com/Nihility981/p-roll-cursor/internal/jwtutil"
	"github.com/Nihility981/p-roll-cursor/internal/vscdb"
)

// FromItems 用 state.vscdb 里的原始 key/value 构造一条账号记录。
//
// 刻意只接受 map 而不是数据库句柄：这样单元测试不需要真有一个 state.vscdb，
// 也不会有任何碰 Cursor 文件的可能。
func FromItems(items map[string]string, now time.Time) (Account, error) {
	token := strings.TrimSpace(items[vscdb.KeyAccessToken])
	if token == "" {
		return Account{}, fmt.Errorf("缺少 %s，无法判断这是哪个账号", vscdb.KeyAccessToken)
	}

	payload, err := jwtutil.Decode(token)
	if err != nil {
		return Account{}, fmt.Errorf("解析 %s 失败: %w", vscdb.KeyAccessToken, err)
	}

	// 用 sub 切出来的用户 ID 做主键，不用邮箱：邮箱可以改，用户 ID 不会变。
	userID, _ := jwtutil.WorkosUserIDFromSub(payload.Sub)
	if strings.TrimSpace(userID) == "" {
		return Account{}, fmt.Errorf("无法从 JWT sub %q 推出用户 ID", payload.Sub)
	}

	// 复制一份，避免调用方之后改动 map 影响到已构造的记录。
	snapshot := make(map[string]string, len(items))
	for k, v := range items {
		snapshot[k] = v
	}

	acc := Account{
		UserID:         userID,
		Email:          strings.TrimSpace(items[vscdb.KeyCachedEmail]),
		MembershipType: strings.TrimSpace(items[vscdb.KeyStripeMembershipType]),
		SignUpType:     strings.TrimSpace(items[vscdb.KeyCachedSignUpType]),
		Sub:            payload.Sub,
		ExportedAt:     now,
		Items:          snapshot,
	}
	if payload.Exp != 0 {
		exp := payload.ExpiresAt()
		acc.TokenExpiresAt = &exp
	}
	return acc, nil
}

// FromAuthState 从只读读出的登录态构造账号记录，只收录实际存在的 key。
func FromAuthState(state *vscdb.AuthState, now time.Time) (Account, error) {
	if state == nil {
		return Account{}, fmt.Errorf("登录态为空")
	}
	items := make(map[string]string, len(state.Items))
	for key, item := range state.Items {
		if item.Exists {
			items[key] = item.Value
		}
	}
	return FromItems(items, now)
}

// UserIDLooksStandard 报告用户 ID 是否是预期的 user_ 前缀形态。
// 本机实测 sub 为 auth0|user_01KY...，切完是 user_ 开头；
// 若哪天不是，调用方应当把它当成需要人工确认的信号，而不是静默接受。
func UserIDLooksStandard(userID string) bool {
	return strings.HasPrefix(userID, "user_")
}
