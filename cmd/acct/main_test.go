package main

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Nihility981/p-roll-cursor/internal/acctstore"
	"github.com/Nihility981/p-roll-cursor/internal/vscdb"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// fixtureDB 建一个临时库，建表语句与 Cursor 的 state.vscdb 完全一致，并塞进「旧账号」。
//
// 所有写入测试都跑在 t.TempDir() 里的库上。真实 state.vscdb 绝不参与测试：
// 用户正开着 Cursor，写下去会毁掉他的登录态。
func fixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.vscdb")

	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("建临时库失败: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("开 WAL 失败: %v", err)
	}

	old := map[string]string{
		vscdb.KeyAccessToken:            makeToken(t, "auth0|user_OLD", time.Now().Add(48*time.Hour)),
		vscdb.KeyRefreshToken:           makeToken(t, "auth0|user_OLD", time.Now().Add(48*time.Hour)),
		vscdb.KeyCachedEmail:            "old@example.com",
		vscdb.KeyCachedSignUpType:       "Auth_0",
		vscdb.KeyStripeMembershipType:   "free",
		vscdb.KeyStripeMembershipAuthID: "auth0|user_OLD",
		vscdb.KeyCachedScopedProfile:    `{"displayName":"Example User"}`,
		vscdb.KeyOnboardingDate:         "2025-12-03T15:55:37.820Z",
	}
	for k, v := range old {
		if _, err := db.Exec(`INSERT INTO ItemTable(key, value) VALUES(?, ?)`, k, v); err != nil {
			t.Fatalf("塞初始数据失败: %v", err)
		}
	}
	return path
}

func makeToken(t *testing.T, sub string, exp time.Time) string {
	t.Helper()
	payload := fmt.Sprintf(`{"sub":%q,"exp":%d}`, sub, exp.Unix())
	return "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

type row struct {
	Value  string
	TypeOf string
}

// dumpDB 用裸 SQL 读回，避免用被测代码验证被测代码。
func dumpDB(t *testing.T, path string) map[string]row {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT key, value, typeof(value) FROM ItemTable`)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	defer rows.Close()

	out := map[string]row{}
	for rows.Next() {
		var k, typ string
		var v []byte
		if err := rows.Scan(&k, &v, &typ); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		out[k] = row{Value: string(v), TypeOf: typ}
	}
	return out
}

func storePathIn(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "accounts.json")
}

// TestSwitchTokenShortcut 一条命令直切：从 token 构造账号 → 存库 → 写 state.vscdb。
func TestSwitchTokenShortcut(t *testing.T) {
	db := fixtureDB(t)
	store := storePathIn(t)
	tok := makeToken(t, "auth0|user_NEW9Z8Y7X6", time.Now().Add(72*time.Hour))

	err := runSwitch([]string{
		"-token", tok,
		"-email", "new@example.com",
		"-membership", "pro",
		"-store", store,
		"-db", db,
	})
	if err != nil {
		t.Fatalf("token 直切失败: %v", err)
	}

	after := dumpDB(t, db)

	// 6 个 key 写进去了，而且必须是 text 而不是 blob。
	for _, key := range vscdb.SwitchWriteKeys {
		r, ok := after[key]
		if !ok {
			t.Errorf("key %q 应当被写入", key)
			continue
		}
		if r.TypeOf != "text" {
			t.Errorf("key %q 的 typeof 期望 text，实际 %q —— 会被 Cursor 读成 Buffer", key, r.TypeOf)
		}
	}
	if after[vscdb.KeyAccessToken].Value != tok {
		t.Error("accessToken 应当就是命令行给的那个 token")
	}
	if after[vscdb.KeyCachedEmail].Value != "new@example.com" {
		t.Errorf("邮箱期望 new@example.com，实际 %q", after[vscdb.KeyCachedEmail].Value)
	}
	if after[vscdb.KeyStripeMembershipType].Value != "pro" {
		t.Errorf("套餐期望 pro，实际 %q", after[vscdb.KeyStripeMembershipType].Value)
	}
	// stripeMembershipAuthId 应当从 JWT 的 sub 推出来。
	if after[vscdb.KeyStripeMembershipAuthID].Value != "auth0|user_NEW9Z8Y7X6" {
		t.Errorf("authId 应当来自 JWT 的 sub，实际 %q", after[vscdb.KeyStripeMembershipAuthID].Value)
	}

	// 2 个账号绑定缓存被删掉了。
	for _, key := range vscdb.SwitchDeleteKeys {
		if _, ok := after[key]; ok {
			t.Errorf("key %q 应当被删除，实际仍在库里", key)
		}
	}

	// 账号也存进了账号库，等价于跑过一次 import。
	f, err := acctstore.Open(store)
	if err != nil {
		t.Fatalf("打开账号库失败: %v", err)
	}
	loaded, err := f.Load()
	if err != nil {
		t.Fatalf("读账号库失败: %v", err)
	}
	if len(loaded.Accounts) != 1 {
		t.Fatalf("账号库应有 1 个账号，实际 %d", len(loaded.Accounts))
	}
	if got := loaded.Accounts[0].UserID; got != "user_NEW9Z8Y7X6" {
		t.Errorf("用户 ID 期望 user_NEW9Z8Y7X6，实际 %q", got)
	}
	if loaded.Accounts[0].Email != "new@example.com" {
		t.Errorf("账号库里的邮箱不对: %q", loaded.Accounts[0].Email)
	}

	// 旧值备份存在，且不是整库拷贝。
	backups, err := filepath.Glob(filepath.Join(backupDir(store), "switch-*.json"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("应生成 1 份旧值备份，实际 %v (err=%v)", backups, err)
	}
	if st, _ := os.Stat(backups[0]); st != nil && st.Size() > 64*1024 {
		t.Errorf("备份 %d 字节，过大，疑似整库备份", st.Size())
	}
}

// TestSwitchTokenOverwritesSameUser 同一用户 ID 再次直切时覆盖成新 token，不报错。
func TestSwitchTokenOverwritesSameUser(t *testing.T) {
	db := fixtureDB(t)
	store := storePathIn(t)

	first := makeToken(t, "auth0|user_SAME", time.Now().Add(24*time.Hour))
	second := makeToken(t, "auth0|user_SAME", time.Now().Add(240*time.Hour))

	for _, tok := range []string{first, second} {
		if err := runSwitch([]string{"-token", tok, "-email", "same@example.com",
			"-store", store, "-db", db}); err != nil {
			t.Fatalf("直切失败: %v", err)
		}
	}

	f, _ := acctstore.Open(store)
	loaded, err := f.Load()
	if err != nil {
		t.Fatalf("读账号库失败: %v", err)
	}
	if len(loaded.Accounts) != 1 {
		t.Errorf("同一用户 ID 不应产生两条记录，实际 %d 条", len(loaded.Accounts))
	}
	if loaded.Accounts[0].Items[vscdb.KeyAccessToken] != second {
		t.Error("账号库里应当是后一个（更新的）token")
	}
	if dumpDB(t, db)[vscdb.KeyAccessToken].Value != second {
		t.Error("state.vscdb 里应当是后一个 token")
	}
}

// TestSwitchTargetMutuallyExclusive 位置参数与 -token 二选一。
func TestSwitchTargetMutuallyExclusive(t *testing.T) {
	db := fixtureDB(t)
	store := storePathIn(t)
	tok := makeToken(t, "auth0|user_X", time.Now().Add(time.Hour))

	err := runSwitch([]string{"someone@example.com", "-token", tok, "-store", store, "-db", db})
	if err == nil || !strings.Contains(err.Error(), "二选一") {
		t.Errorf("同时给出两者应报错，实际 %v", err)
	}

	err = runSwitch([]string{"-store", store, "-db", db})
	if err == nil || !strings.Contains(err.Error(), "两种用法") {
		t.Errorf("两者都不给应说明两种用法，实际 %v", err)
	}

	// 报错时库必须没动过。
	if dumpDB(t, db)[vscdb.KeyCachedEmail].Value != "old@example.com" {
		t.Error("参数报错时不应改动数据库")
	}
}

// TestSwitchPositionalStillWorks 原有的「先 import 再 switch」两步用法不受影响。
func TestSwitchPositionalStillWorks(t *testing.T) {
	db := fixtureDB(t)
	store := storePathIn(t)
	tok := makeToken(t, "auth0|user_TWOSTEP", time.Now().Add(72*time.Hour))

	if err := runImport([]string{"-token", tok, "-email", "two@example.com",
		"-membership", "enterprise", "-store", store}); err != nil {
		t.Fatalf("import 失败: %v", err)
	}
	// 既能用邮箱指定，也能用用户 ID。
	if err := runSwitch([]string{"two@example.com", "-store", store, "-db", db}); err != nil {
		t.Fatalf("按邮箱切号失败: %v", err)
	}
	after := dumpDB(t, db)
	if after[vscdb.KeyCachedEmail].Value != "two@example.com" {
		t.Errorf("邮箱应已切换，实际 %q", after[vscdb.KeyCachedEmail].Value)
	}
	if after[vscdb.KeyAccessToken].TypeOf != "text" {
		t.Errorf("typeof 应为 text，实际 %q", after[vscdb.KeyAccessToken].TypeOf)
	}

	if err := runSwitch([]string{"user_TWOSTEP", "-store", store, "-db", db}); err != nil {
		t.Fatalf("按用户 ID 切号失败: %v", err)
	}
}

// TestSwitchTokenDryRunWritesNothing -dry-run 不写 state.vscdb，也不写账号库。
func TestSwitchTokenDryRunWritesNothing(t *testing.T) {
	db := fixtureDB(t)
	before := dumpDB(t, db)
	store := storePathIn(t)
	tok := makeToken(t, "auth0|user_DRY", time.Now().Add(72*time.Hour))

	if err := runSwitch([]string{"-token", tok, "-email", "dry@example.com",
		"-store", store, "-db", db, "-dry-run"}); err != nil {
		t.Fatalf("dry-run 应当成功返回: %v", err)
	}

	if fmt.Sprint(dumpDB(t, db)) != fmt.Sprint(before) {
		t.Error("-dry-run 不应改动 state.vscdb")
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("-dry-run 不应创建账号库文件，实际 stat err=%v", err)
	}
	if backups, _ := filepath.Glob(filepath.Join(backupDir(store), "*")); len(backups) > 0 {
		t.Errorf("-dry-run 不应生成备份，实际 %v", backups)
	}
}

// TestSwitchTokenRejectsExpired 直切时过期 token 同样默认被拒。
func TestSwitchTokenRejectsExpired(t *testing.T) {
	db := fixtureDB(t)
	store := storePathIn(t)
	expired := makeToken(t, "auth0|user_OLDTOK", time.Now().Add(-2*time.Hour))

	err := runSwitch([]string{"-token", expired, "-store", store, "-db", db})
	if err == nil || !strings.Contains(err.Error(), "过期") ||
		!strings.Contains(err.Error(), "-allow-expired") {
		t.Errorf("过期 token 应默认拒绝并给出绕过办法，实际 %v", err)
	}
	if dumpDB(t, db)[vscdb.KeyCachedEmail].Value != "old@example.com" {
		t.Error("被拒绝时不应改动数据库")
	}
	// 注意：此时 token 已经存进账号库了（存库在写库之前），所以用户可以直接
	// `acct switch <邮箱> -allow-expired` 重试，不必再粘一遍 token。
	if f, _ := acctstore.Open(store); f != nil {
		if loaded, err := f.Load(); err == nil && len(loaded.Accounts) != 1 {
			t.Errorf("被拒绝的 token 仍应已存入账号库以便重试，实际 %d 条", len(loaded.Accounts))
		}
	}

	if err := runSwitch([]string{"-token", expired, "-store", store, "-db", db,
		"-allow-expired"}); err != nil {
		t.Fatalf("显式允许过期后应当成功: %v", err)
	}
	if dumpDB(t, db)[vscdb.KeyAccessToken].Value != expired {
		t.Error("显式允许后应已写入")
	}
}

// TestSwitchTokenGarbage 不是 JWT 的 token 要给出可理解的错误，而不是写进去。
func TestSwitchTokenGarbage(t *testing.T) {
	db := fixtureDB(t)
	store := storePathIn(t)

	if err := runSwitch([]string{"-token", "   ", "-store", store, "-db", db}); err == nil {
		t.Error("空 token 应报错")
	}
	if err := runSwitch([]string{"-token", "not-a-jwt", "-store", store, "-db", db}); err == nil {
		t.Error("非 JWT 应报错")
	}
	if dumpDB(t, db)[vscdb.KeyCachedEmail].Value != "old@example.com" {
		t.Error("token 无效时不应改动数据库")
	}
}

// TestAccountFromTokenSharedWithImport 两条路径必须解析出同一个账号。
func TestAccountFromTokenSharedWithImport(t *testing.T) {
	tok := makeToken(t, "auth0|user_SHARED", time.Now().Add(time.Hour))
	now := time.Now()

	viaHelper, err := accountFromToken(tok, "a@b.com", "pro", "Auth_0", now)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	viaItems, err := acctstore.FromItems(itemsFromToken(tok, "a@b.com", "pro", "Auth_0"), now)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	if viaHelper.UserID != viaItems.UserID || viaHelper.Email != viaItems.Email ||
		len(viaHelper.Items) != len(viaItems.Items) {
		t.Error("accountFromToken 与 import 的构造结果不一致")
	}
	if viaHelper.UserID != "user_SHARED" {
		t.Errorf("用户 ID 应取 sub 按 | 切最后一段，实际 %q", viaHelper.UserID)
	}
}

// TestUsageMentionsAt --help 最快用法应是 acct at，不再提独立脚本。
func TestUsageMentionsAt(t *testing.T) {
	if !strings.Contains(usageText, "acct at") {
		t.Error("usage 应包含 acct at")
	}
	if !strings.Contains(usageText, "acct at <accessToken>") {
		t.Error("usage 应包含 acct at <accessToken>")
	}
	if strings.Contains(usageText, "at.bat") || strings.Contains(usageText, "at.ps1") {
		t.Error("usage 不应再提 at.bat / at.ps1")
	}
}

// TestAtRejectsEmptyToken 没给 token（参数空白或 stdin 空行）必须报错，不能往下走切号。
func TestAtRejectsEmptyToken(t *testing.T) {
	_, _, err := resolveAtToken([]string{}, strings.NewReader("\n"))
	if err == nil || !strings.Contains(err.Error(), "为空") {
		t.Errorf("stdin 空行应报 token 为空，实际 %v", err)
	}

	_, _, err = resolveAtToken([]string{"   "}, strings.NewReader("should-not-be-read"))
	if err == nil || !strings.Contains(err.Error(), "为空") {
		t.Errorf("空白位置参数应报 token 为空，实际 %v", err)
	}

	_, _, err = resolveAtToken(nil, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "为空") {
		t.Errorf("stdin EOF 应报 token 为空，实际 %v", err)
	}
}

// TestAtReusesSwitchArgs 有 token 时只构造等价的 switch 参数，不在测试里真的 -kill -start。
func TestAtReusesSwitchArgs(t *testing.T) {
	got := atSwitchArgs("eyJabc", nil)
	want := []string{"-token", "eyJabc", "-kill", "-start"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("at 应构造与 switch -token -kill -start 等价的参数，实际 %v", got)
	}

	got = atSwitchArgs("tok", []string{"-dry-run", "-store", "x"})
	want = []string{"-token", "tok", "-kill", "-start", "-dry-run", "-store", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("额外参数应追加在 -kill -start 之后，实际 %v", got)
	}

	tok, extra, err := resolveAtToken([]string{"eyJhello", "-store", "p"}, nil)
	if err != nil {
		t.Fatalf("从参数读 token 失败: %v", err)
	}
	if tok != "eyJhello" {
		t.Errorf("token 期望 eyJhello，实际 %q", tok)
	}
	if !reflect.DeepEqual(extra, []string{"-store", "p"}) {
		t.Errorf("其余 flag 应原样留下，实际 %v", extra)
	}
	if !reflect.DeepEqual(atSwitchArgs(tok, extra), []string{"-token", "eyJhello", "-kill", "-start", "-store", "p"}) {
		t.Error("解析结果交给 atSwitchArgs 后应仍是同一套 switch 准备")
	}

	tok, extra, err = resolveAtToken([]string{"-store", "p"}, strings.NewReader("eyJfromstdin\n"))
	if err != nil {
		t.Fatalf("从 stdin 读 token 失败: %v", err)
	}
	if tok != "eyJfromstdin" {
		t.Errorf("stdin token 期望 eyJfromstdin，实际 %q", tok)
	}
	if !reflect.DeepEqual(extra, []string{"-store", "p"}) {
		t.Errorf("无位置参数时 flag 应全部留下，实际 %v", extra)
	}
}
