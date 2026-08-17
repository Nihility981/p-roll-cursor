package acctstore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nihility981/p-roll-cursor/internal/vscdb"
)

// makeJWT 造一个只有 payload 有意义的 JWT——签名不校验，够用。
func makeJWT(sub string, exp int64) string {
	payload := fmt.Sprintf(`{"sub":%q,"exp":%d,"iss":"https://authentication.cursor.sh"}`, sub, exp)
	seg := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + seg + ".fakesignature"
}

// realWorldItems 复刻本机 state.vscdb 里实际存在的那 8 个 key。
func realWorldItems(sub string, exp int64) map[string]string {
	token := makeJWT(sub, exp)
	return map[string]string{
		vscdb.KeyAccessToken:            token,
		vscdb.KeyRefreshToken:           token, // 实测两者相同
		vscdb.KeyCachedEmail:            "someone@example.com",
		vscdb.KeyCachedSignUpType:       "Auth_0",
		vscdb.KeyStripeMembershipType:   "enterprise",
		vscdb.KeyStripeMembershipAuthID: sub,
		vscdb.KeyCachedScopedProfile:    `{"profile":"x"}`,
		vscdb.KeyOnboardingDate:         "2025-01-01",
	}
}

func tempStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "accounts.json"), JSONCodec{})
}

// TestRoundTrip 写入后读回必须完全一致，包括原始 key/value 全量。
func TestRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	exp := now.Add(48 * time.Hour).Unix()

	acc, err := FromItems(realWorldItems("auth0|user_00000000000000000000000000", exp), now)
	if err != nil {
		t.Fatalf("构造账号记录失败: %v", err)
	}

	s := tempStore(t)
	if _, err := s.Upsert(acc, ConflictReject); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	f, err := s.Load()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(f.Accounts) != 1 {
		t.Fatalf("应有 1 条记录，实际 %d", len(f.Accounts))
	}
	got := f.Accounts[0]

	if got.UserID != "user_00000000000000000000000000" {
		t.Errorf("userId 期望切出 user_ 段，实际 %q", got.UserID)
	}
	if got.Email != "someone@example.com" || got.MembershipType != "enterprise" || got.SignUpType != "Auth_0" {
		t.Errorf("派生字段不对: %+v", got)
	}
	if got.Sub != "auth0|user_00000000000000000000000000" {
		t.Errorf("应保留原始 sub，实际 %q", got.Sub)
	}
	if got.TokenExpiresAt == nil || got.TokenExpiresAt.Unix() != exp {
		t.Errorf("exp 期望 %d，实际 %v", exp, got.TokenExpiresAt)
	}
	if !got.ExportedAt.Equal(now) {
		t.Errorf("导出时间期望 %v，实际 %v", now, got.ExportedAt)
	}

	// 原始 key/value 必须全量保留，一个都不能丢。
	want := realWorldItems("auth0|user_00000000000000000000000000", exp)
	if len(got.Items) != len(want) {
		t.Fatalf("原始 key 数量期望 %d，实际 %d", len(want), len(got.Items))
	}
	for k, v := range want {
		if got.Items[k] != v {
			t.Errorf("key %q 的值不一致：期望 %q，实际 %q", k, v, got.Items[k])
		}
	}

	// 文件头部元信息。
	if f.Version != FormatVersion {
		t.Errorf("版本期望 %d，实际 %d", FormatVersion, f.Version)
	}
	if f.Encryption != "none" {
		t.Errorf("当前是明文实现，Encryption 应为 none，实际 %q", f.Encryption)
	}
}

// TestFromItemsSnapshotIsCopied 调用方之后改 map 不应影响已构造的记录。
func TestFromItemsSnapshotIsCopied(t *testing.T) {
	items := realWorldItems("auth0|user_A", time.Now().Add(time.Hour).Unix())
	acc, err := FromItems(items, time.Now())
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	items[vscdb.KeyCachedEmail] = "tampered@example.com"

	if acc.Items[vscdb.KeyCachedEmail] == "tampered@example.com" {
		t.Error("账号记录应持有独立副本，不应被调用方后续改动影响")
	}
}

// TestDuplicateRejectedByDefault 默认策略是拒绝，且错误信息要能帮人做决定。
func TestDuplicateRejectedByDefault(t *testing.T) {
	now := time.Now()
	sub := "auth0|user_DUP"
	acc, _ := FromItems(realWorldItems(sub, now.Add(time.Hour).Unix()), now)

	s := tempStore(t)
	if _, err := s.Upsert(acc, ConflictReject); err != nil {
		t.Fatalf("首次写入不该失败: %v", err)
	}

	_, err := s.Upsert(acc, ConflictReject)
	if err == nil {
		t.Fatal("同一 userId 重复导出应当被拒绝")
	}
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("应可用 errors.Is 判定为重复，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "user_DUP") {
		t.Errorf("错误信息应包含 userId，实际 %q", err.Error())
	}

	// 被拒绝后库里仍应只有一条，且没有被改坏。
	f, err := s.Load()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(f.Accounts) != 1 {
		t.Errorf("拒绝后应仍只有 1 条，实际 %d", len(f.Accounts))
	}
}

// TestDuplicateOverwrite 显式指定覆盖时，旧记录被替换并回传。
func TestDuplicateOverwrite(t *testing.T) {
	now := time.Now()
	sub := "auth0|user_DUP"
	old, _ := FromItems(realWorldItems(sub, now.Add(time.Hour).Unix()), now.Add(-24*time.Hour))

	s := tempStore(t)
	if _, err := s.Upsert(old, ConflictReject); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}

	fresh, _ := FromItems(realWorldItems(sub, now.Add(72*time.Hour).Unix()), now)
	res, err := s.Upsert(fresh, ConflictOverwrite)
	if err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	if !res.Updated || res.Created {
		t.Errorf("应标记为更新而非新建: %+v", res)
	}
	if !res.Previous.ExportedAt.Equal(old.ExportedAt) {
		t.Error("应回传被覆盖的旧记录，便于调用方展示")
	}

	f, _ := s.Load()
	if len(f.Accounts) != 1 {
		t.Fatalf("覆盖后仍应只有 1 条，实际 %d", len(f.Accounts))
	}
	if f.Accounts[0].TokenExpiresAt.Unix() != fresh.TokenExpiresAt.Unix() {
		t.Error("覆盖后应是新记录的过期时间")
	}
}

// TestDifferentUsersCoexist 不同 userId 应共存，且邮箱相同也不算重复。
func TestDifferentUsersCoexist(t *testing.T) {
	now := time.Now()
	s := tempStore(t)

	for _, sub := range []string{"auth0|user_B", "auth0|user_A"} {
		acc, err := FromItems(realWorldItems(sub, now.Add(time.Hour).Unix()), now)
		if err != nil {
			t.Fatalf("构造 %s 失败: %v", sub, err)
		}
		if _, err := s.Upsert(acc, ConflictReject); err != nil {
			t.Fatalf("写入 %s 失败: %v", sub, err)
		}
	}

	f, _ := s.Load()
	if len(f.Accounts) != 2 {
		t.Fatalf("应有 2 条记录，实际 %d", len(f.Accounts))
	}
	// 落盘时按 userId 排序，保证多次写入后文件内容稳定、可 diff。
	if f.Accounts[0].UserID != "user_A" || f.Accounts[1].UserID != "user_B" {
		t.Errorf("应按 userId 排序，实际 %s, %s", f.Accounts[0].UserID, f.Accounts[1].UserID)
	}
}

// TestAtomicWriteFailureKeepsOriginal 是最关键的一条：
// 临时文件已写好但替换失败时，原账号库必须完好无损，且不留临时文件垃圾。
func TestAtomicWriteFailureKeepsOriginal(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	s := New(path, JSONCodec{})

	good, _ := FromItems(realWorldItems("auth0|user_KEEP", now.Add(time.Hour).Unix()), now)
	if _, err := s.Upsert(good, ConflictReject); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读原文件失败: %v", err)
	}

	// 模拟替换环节失败。
	original := renameFile
	renameFile = func(string, string) error { return errors.New("模拟断电") }
	defer func() { renameFile = original }()

	other, _ := FromItems(realWorldItems("auth0|user_NEW", now.Add(time.Hour).Unix()), now)
	if _, err := s.Upsert(other, ConflictReject); err == nil {
		t.Fatal("替换失败时 Upsert 必须报错")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("原文件不应消失: %v", err)
	}
	if string(before) != string(after) {
		t.Error("替换失败后原账号库被改动了，原子写没有生效")
	}

	// 不该留下临时文件。
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("失败后残留了临时文件 %s", e.Name())
		}
	}
}

// TestLoadMissingFileIsEmptyStore 首次使用时文件不存在，应视为空库而非错误。
func TestLoadMissingFileIsEmptyStore(t *testing.T) {
	s := tempStore(t)
	f, err := s.Load()
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if len(f.Accounts) != 0 {
		t.Errorf("应是空库，实际 %d 条", len(f.Accounts))
	}
}

// TestLoadCorruptedFile 损坏或空文件不能 panic，要给出可理解的错误。
func TestLoadCorruptedFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantIn  string
	}{
		{"截断的 JSON", `{"version":1,"accounts":[{"userId":"user_A"`, "损坏"},
		{"根本不是 JSON", "这不是 JSON", "损坏"},
		{"空文件", "", "空文件"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("准备文件失败: %v", err)
			}
			s := New(path, JSONCodec{})

			_, err := s.Load()
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("错误信息应包含 %q，实际 %q", tc.wantIn, err.Error())
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("错误信息应包含文件路径，便于定位，实际 %q", err.Error())
			}
		})
	}
}

// TestLoadFutureVersion 格式版本高于本程序时要明确拒绝，不能瞎解析。
func TestLoadFutureVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	body, _ := json.Marshal(File{Version: FormatVersion + 1, Encryption: "none"})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	_, err := New(path, JSONCodec{}).Load()
	if err == nil || !strings.Contains(err.Error(), "格式版本") {
		t.Errorf("应提示格式版本过高，实际 %v", err)
	}
}

// TestExpiryBoundary 过期判定的边界。
func TestExpiryBoundary(t *testing.T) {
	exp := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	acc := Account{UserID: "user_X", TokenExpiresAt: &exp}

	cases := []struct {
		name    string
		now     time.Time
		expired bool
	}{
		{"过期前一秒", exp.Add(-time.Second), false},
		{"恰好等于 exp 那一刻仍算有效", exp, false},
		{"过期后一秒", exp.Add(time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acc.IsExpired(tc.now); got != tc.expired {
				t.Errorf("IsExpired 期望 %v，实际 %v", tc.expired, got)
			}
		})
	}

	// 没有 exp 时：「不知道」不等于「已过期」。
	unknown := Account{UserID: "user_Y"}
	if unknown.IsExpired(time.Now()) {
		t.Error("缺少 exp 时不应判为已过期")
	}
	if _, ok := unknown.ExpiresIn(time.Now()); ok {
		t.Error("缺少 exp 时 ExpiresIn 应返回 false")
	}
	if d, ok := acc.ExpiresIn(exp.Add(-time.Hour)); !ok || d != time.Hour {
		t.Errorf("ExpiresIn 期望 1h，实际 %v ok=%v", d, ok)
	}
}

// TestFromItemsRejectsBadInput 缺 token 或 JWT 无法解析时要报清楚，不能静默产出空记录。
func TestFromItemsRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		items  map[string]string
		wantIn string
	}{
		{"完全没有 accessToken", map[string]string{}, vscdb.KeyAccessToken},
		{"accessToken 是空串", map[string]string{vscdb.KeyAccessToken: "   "}, vscdb.KeyAccessToken},
		{"不是合法 JWT", map[string]string{vscdb.KeyAccessToken: "not-a-jwt"}, "解析"},
		{"sub 为空", map[string]string{vscdb.KeyAccessToken: makeJWT("", 0)}, "用户 ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromItems(tc.items, time.Now())
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("错误信息应包含 %q，实际 %q", tc.wantIn, err.Error())
			}
		})
	}
}

// TestFromItemsWithoutExp 没有 exp 的 token 也要能入库，只是过期时间未知。
func TestFromItemsWithoutExp(t *testing.T) {
	acc, err := FromItems(map[string]string{
		vscdb.KeyAccessToken: makeJWT("auth0|user_NOEXP", 0),
	}, time.Now())
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if acc.TokenExpiresAt != nil {
		t.Errorf("没有 exp 时应为 nil，实际 %v", acc.TokenExpiresAt)
	}
	if acc.UserID != "user_NOEXP" {
		t.Errorf("userId 期望 user_NOEXP，实际 %q", acc.UserID)
	}
}

// TestUserIDLooksStandard 非 user_ 前缀要能被识别出来，交给人确认。
func TestUserIDLooksStandard(t *testing.T) {
	if !UserIDLooksStandard("user_00000000000000000000000000") {
		t.Error("本机实测形态应被认为正常")
	}
	if UserIDLooksStandard("auth0|somethingelse") {
		t.Error("非 user_ 前缀应被识别为异常")
	}
}

// TestResolvePathOverride 覆盖机制：显式参数 > 环境变量 > 默认位置。
func TestResolvePathOverride(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "explicit.json")
	envPath := filepath.Join(t.TempDir(), "from-env.json")

	t.Setenv(EnvStorePath, envPath)

	if got, err := ResolvePath(explicit); err != nil || got != explicit {
		t.Errorf("显式参数应优先，期望 %q，实际 %q (err=%v)", explicit, got, err)
	}
	if got, err := ResolvePath(""); err != nil || got != envPath {
		t.Errorf("应采用环境变量，期望 %q，实际 %q (err=%v)", envPath, got, err)
	}

	t.Setenv(EnvStorePath, "")
	got, err := ResolvePath("")
	if err != nil {
		t.Fatalf("默认路径解析失败: %v", err)
	}
	// 默认位置必须在用户目录下，且带上 cursor-switch 这一层，
	// 保证它落在仓库之外——账号库里是 refresh token，绝不能进版本库。
	if !strings.Contains(filepath.ToSlash(got), "cursor-switch/accounts.json") {
		t.Errorf("默认路径形态不对: %q", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("默认路径应是绝对路径，实际 %q", got)
	}
}

// TestSaveCreatesDirectory 目录不存在时应自动创建。
func TestSaveCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "accounts.json")
	s := New(path, JSONCodec{})

	acc, _ := FromItems(realWorldItems("auth0|user_MKDIR", time.Now().Add(time.Hour).Unix()), time.Now())
	if _, err := s.Upsert(acc, ConflictReject); err != nil {
		t.Fatalf("应自动建目录并写入，实际失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("文件应已创建: %v", err)
	}
}

// TestCodecIsSwappable 证明换编解码器不需要改调用方——将来加密时就靠这个。
func TestCodecIsSwappable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.bin")
	s := New(path, reverseCodec{})

	acc, _ := FromItems(realWorldItems("auth0|user_CODEC", time.Now().Add(time.Hour).Unix()), time.Now())
	if _, err := s.Upsert(acc, ConflictReject); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "user_CODEC") {
		t.Error("换了编解码器后落盘内容不应还是原样 JSON")
	}

	f, err := s.Load()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].UserID != "user_CODEC" {
		t.Errorf("往返后数据应一致，实际 %+v", f.Accounts)
	}
	if f.Encryption != "reverse" {
		t.Errorf("Encryption 应记录编解码器名字，实际 %q", f.Encryption)
	}
}

// reverseCodec 是个假的「加密」实现：把 JSON 字节倒过来。
// 它不提供任何安全性，只用来证明 Codec 接口确实可替换。
type reverseCodec struct{}

func (reverseCodec) Name() string { return "reverse" }

func (reverseCodec) Encode(f File) ([]byte, error) {
	data, err := JSONCodec{}.Encode(f)
	if err != nil {
		return nil, err
	}
	return reverseBytes(data), nil
}

func (reverseCodec) Decode(data []byte) (File, error) {
	return JSONCodec{}.Decode(reverseBytes(data))
}

func reverseBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}
