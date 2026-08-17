package switcher

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nihility981/p-roll-cursor/internal/acctstore"
	"github.com/Nihility981/p-roll-cursor/internal/vscdb"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// newTestDB 建一个临时库，建表语句与 Cursor 的 state.vscdb 完全一致
// （key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB），并开启 WAL。
//
// 真实的 state.vscdb 绝不能用来做写入测试：用户正开着 Cursor，写下去会毁掉
// 他的登录态。这里所有写入都发生在 t.TempDir() 里的临时库上。
func newTestDB(t *testing.T) string {
	t.Helper()
	return makeDBAt(t, filepath.Join(t.TempDir(), "state.vscdb"))
}

// TestMakeFixtureDB 不是断言，而是给手工验证造一个与 Cursor 建表语句相同的库。
// 只有设置了 SWITCHER_FIXTURE_DB 时才会执行，平时自动跳过。
// 这样手工跑 acct switch -db <path> 就能在临时库上完整演练，不必碰真库。
func TestMakeFixtureDB(t *testing.T) {
	path := os.Getenv("SWITCHER_FIXTURE_DB")
	if path == "" {
		t.Skip("未设置 SWITCHER_FIXTURE_DB，跳过")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	_ = os.Remove(path)
	makeDBAt(t, path)
	t.Logf("已生成夹具库: %s", path)
}

// TestDumpFixtureDB 同样只在设置了 SWITCHER_FIXTURE_DB 时执行，
// 用裸 SQL 把库内容连同 typeof(value) 打出来，供手工验证时独立核对。
func TestDumpFixtureDB(t *testing.T) {
	path := os.Getenv("SWITCHER_FIXTURE_DB")
	if path == "" {
		t.Skip("未设置 SWITCHER_FIXTURE_DB，跳过")
	}
	for key, row := range readAll(t, path) {
		v := row.Value
		if len(v) > 48 {
			v = v[:48] + fmt.Sprintf("...(%d 字符)", len(row.Value))
		}
		t.Logf("%-40s typeof=%-5s %s", key, row.TypeOf, v)
	}
}

func makeDBAt(t *testing.T, path string) string {
	t.Helper()

	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("建临时库失败: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("开启 WAL 失败: %v", err)
	}

	// 塞进「旧账号」的真实形态数据，8 个 key 全都有。
	for key, val := range oldAccountItems() {
		if _, err := db.Exec(`INSERT INTO ItemTable(key, value) VALUES(?, ?)`, key, val); err != nil {
			t.Fatalf("写入初始数据失败: %v", err)
		}
	}
	return path
}

func oldAccountItems() map[string]string {
	tok := makeJWT("auth0|user_OLDACCOUNT", time.Now().Add(48*time.Hour).Unix())
	return map[string]string{
		vscdb.KeyAccessToken:            tok,
		vscdb.KeyRefreshToken:           tok,
		vscdb.KeyCachedEmail:            "old@example.com",
		vscdb.KeyCachedSignUpType:       "Auth_0",
		vscdb.KeyStripeMembershipType:   "free",
		vscdb.KeyStripeMembershipAuthID: "auth0|user_OLDACCOUNT",
		vscdb.KeyCachedScopedProfile:    `{"displayName":"Example User"}`,
		vscdb.KeyOnboardingDate:         "2025-12-03T15:55:37.820Z",
	}
}

func makeJWT(sub string, exp int64) string {
	payload := fmt.Sprintf(`{"sub":%q,"exp":%d}`, sub, exp)
	return "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// newAccount 造一个「要切过去的」账号。
func newAccount(t *testing.T, sub string, exp time.Time) acctstore.Account {
	t.Helper()
	tok := makeJWT(sub, exp.Unix())
	items := map[string]string{
		vscdb.KeyAccessToken:            tok,
		vscdb.KeyRefreshToken:           tok,
		vscdb.KeyCachedEmail:            "new@example.com",
		vscdb.KeyCachedSignUpType:       "Auth_0",
		vscdb.KeyStripeMembershipType:   "enterprise",
		vscdb.KeyStripeMembershipAuthID: sub,
		vscdb.KeyCachedScopedProfile:    `{"displayName":"Should Not Be Written"}`,
		vscdb.KeyOnboardingDate:         "2026-01-01T00:00:00.000Z",
	}
	acc, err := acctstore.FromItems(items, time.Now())
	if err != nil {
		t.Fatalf("构造账号失败: %v", err)
	}
	return acc
}

// readAll 直接用裸 SQL 读回，避免用被测代码验证被测代码。
func readAll(t *testing.T, path string) map[string]struct{ Value, TypeOf string } {
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

	out := map[string]struct{ Value, TypeOf string }{}
	for rows.Next() {
		var k, typ string
		var v []byte
		if err := rows.Scan(&k, &v, &typ); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		out[k] = struct{ Value, TypeOf string }{string(v), typ}
	}
	return out
}

func testOptions(t *testing.T, dbPath string) Options {
	t.Helper()
	return Options{
		DBPath:    dbPath,
		BackupDir: filepath.Join(t.TempDir(), "backups"),
		Now:       time.Now(),
		// 测试里没有真的 Cursor，直接放行。
		EnsureCursorStopped: func() error { return nil },
	}
}

// TestSwitchWritesTextNotBlob 是最关键的一条断言。
//
// ItemTable 的 value 列是 BLOB 亲和性，绑参数如果传 []byte 会静默存成 blob，
// Cursor 侧读出来可能变成 Buffer。这里先证明这个坑真实存在（对照组），
// 再断言真正的切号代码落成的是 text。
func TestSwitchWritesTextNotBlob(t *testing.T) {
	dbPath := newTestDB(t)

	// 对照组：直接用 []byte 绑参数，确认它确实会落成 blob。
	func() {
		db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(dbPath))
		if err != nil {
			t.Fatalf("打开失败: %v", err)
		}
		defer db.Close()
		if _, err := db.Exec(`INSERT INTO ItemTable(key, value) VALUES(?, ?)`,
			"probe/bytes", []byte("hello")); err != nil {
			t.Fatalf("对照写入失败: %v", err)
		}
	}()
	if got := readAll(t, dbPath)["probe/bytes"].TypeOf; got != "blob" {
		t.Fatalf("对照组前提不成立：[]byte 应当落成 blob，实际 %q", got)
	}

	acc := newAccount(t, "auth0|user_NEWACCOUNT", time.Now().Add(72*time.Hour))
	rep, err := Switch(context.Background(), acc, testOptions(t, dbPath))
	if err != nil {
		t.Fatalf("切号失败: %v", err)
	}

	after := readAll(t, dbPath)
	for _, key := range vscdb.SwitchWriteKeys {
		row, ok := after[key]
		if !ok {
			t.Errorf("key %q 应当存在", key)
			continue
		}
		if row.TypeOf != "text" {
			t.Errorf("key %q 的 typeof(value) 期望 text，实际 %q —— 会被 Cursor 读成 Buffer", key, row.TypeOf)
		}
	}
	if len(rep.VerifiedText) != len(vscdb.SwitchWriteKeys) {
		t.Errorf("应对全部 %d 个写入 key 做 text 校验，实际 %d",
			len(vscdb.SwitchWriteKeys), len(rep.VerifiedText))
	}
}

// TestSwitchWritesAndDeletesRightKeys 写 6 个、删 2 个，值必须来自新账号。
func TestSwitchWritesAndDeletesRightKeys(t *testing.T) {
	dbPath := newTestDB(t)
	acc := newAccount(t, "auth0|user_NEWACCOUNT", time.Now().Add(72*time.Hour))

	rep, err := Switch(context.Background(), acc, testOptions(t, dbPath))
	if err != nil {
		t.Fatalf("切号失败: %v", err)
	}
	after := readAll(t, dbPath)

	for _, key := range vscdb.SwitchWriteKeys {
		want := acc.Items[key]
		if after[key].Value != want {
			t.Errorf("key %q 期望 %q，实际 %q", key, want, after[key].Value)
		}
	}
	if after[vscdb.KeyCachedEmail].Value != "new@example.com" {
		t.Errorf("邮箱应换成新账号的，实际 %q", after[vscdb.KeyCachedEmail].Value)
	}
	if after[vscdb.KeyStripeMembershipType].Value != "enterprise" {
		t.Errorf("套餐应换成新账号的，实际 %q", after[vscdb.KeyStripeMembershipType].Value)
	}

	// 两个账号绑定缓存必须被删掉，否则新账号会继承旧账号的 profile。
	for _, key := range vscdb.SwitchDeleteKeys {
		if _, ok := after[key]; ok {
			t.Errorf("key %q 应当被删除，实际仍在库里（值 %q）", key, after[key].Value)
		}
	}
	if len(rep.WrittenKeys) != 6 || len(rep.DeletedKeys) != 2 {
		t.Errorf("期望写 6 删 2，实际写 %d 删 %d", len(rep.WrittenKeys), len(rep.DeletedKeys))
	}

	// 不在清单里的 key 不能被误伤。
	if _, ok := after["probe/untouched"]; ok {
		t.Error("不该凭空多出 key")
	}
}

// TestSwitchDoesNotWriteScopedProfileFromStore 即使账号库里存了 profile，也不能写回去。
func TestSwitchDoesNotWriteScopedProfileFromStore(t *testing.T) {
	dbPath := newTestDB(t)
	acc := newAccount(t, "auth0|user_NEWACCOUNT", time.Now().Add(72*time.Hour))

	if _, err := Switch(context.Background(), acc, testOptions(t, dbPath)); err != nil {
		t.Fatalf("切号失败: %v", err)
	}
	if row, ok := readAll(t, dbPath)[vscdb.KeyCachedScopedProfile]; ok {
		t.Errorf("cachedScopedProfile 应被删除而不是写入新账号的值，实际 %q", row.Value)
	}
}

// TestBackupContentAndRollback 备份内容正确，且能完整回滚。
func TestBackupContentAndRollback(t *testing.T) {
	dbPath := newTestDB(t)
	before := readAll(t, dbPath)

	// 制造一个「切号前就不存在」的 key，验证备份用 null 记录它、回滚时删掉它。
	func() {
		db, _ := sql.Open("sqlite3", "file:"+filepath.ToSlash(dbPath))
		defer db.Close()
		if _, err := db.Exec(`DELETE FROM ItemTable WHERE key = ?`, vscdb.KeyOnboardingDate); err != nil {
			t.Fatalf("准备失败: %v", err)
		}
	}()
	delete(before, vscdb.KeyOnboardingDate)

	acc := newAccount(t, "auth0|user_NEWACCOUNT", time.Now().Add(72*time.Hour))
	rep, err := Switch(context.Background(), acc, testOptions(t, dbPath))
	if err != nil {
		t.Fatalf("切号失败: %v", err)
	}

	// 备份文件应存在、带时间戳、内容可读。
	if !strings.HasPrefix(filepath.Base(rep.BackupPath), "switch-") {
		t.Errorf("备份文件名应带 switch- 前缀与时间戳，实际 %q", filepath.Base(rep.BackupPath))
	}
	raw, err := os.ReadFile(rep.BackupPath)
	if err != nil {
		t.Fatalf("读备份失败: %v", err)
	}
	// 备份必须小，不能是整库拷贝。
	if len(raw) > 64*1024 {
		t.Errorf("备份文件 %d 字节，过大，疑似整库备份", len(raw))
	}

	b, err := LoadBackup(rep.BackupPath)
	if err != nil {
		t.Fatalf("解析备份失败: %v", err)
	}
	if b.SwitchedTo != acc.UserID {
		t.Errorf("备份应记录目标账号，实际 %q", b.SwitchedTo)
	}
	if len(b.Items) != len(vscdb.SwitchTouchedKeys()) {
		t.Errorf("备份应覆盖全部 %d 个受影响 key，实际 %d", len(vscdb.SwitchTouchedKeys()), len(b.Items))
	}
	if v, ok := b.Items[vscdb.KeyOnboardingDate]; !ok || v != nil {
		t.Error("切号前不存在的 key，备份里应记为 null")
	}
	if v := b.Items[vscdb.KeyCachedEmail]; v == nil || *v != "old@example.com" {
		t.Errorf("备份里应是旧邮箱，实际 %v", v)
	}

	// 回滚，库应当回到切号前的状态。
	rb, err := Rollback(context.Background(), b, dbPath, nil)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if len(rb.Removed) != 1 || rb.Removed[0] != vscdb.KeyOnboardingDate {
		t.Errorf("原本不存在的 key 回滚时应被删除，实际 %v", rb.Removed)
	}

	after := readAll(t, dbPath)
	for key, want := range before {
		got, ok := after[key]
		if !ok {
			t.Errorf("回滚后 key %q 丢失", key)
			continue
		}
		if got.Value != want.Value {
			t.Errorf("回滚后 key %q 期望 %q，实际 %q", key, want.Value, got.Value)
		}
		if got.TypeOf != "text" {
			t.Errorf("回滚后 key %q 的 typeof 期望 text，实际 %q", key, got.TypeOf)
		}
	}
	if _, ok := after[vscdb.KeyOnboardingDate]; ok {
		t.Error("回滚后该 key 应当不存在")
	}
}

// TestSwitchRefusesWhenCursorRunning Cursor 还在跑时必须拒绝。
func TestSwitchRefusesWhenCursorRunning(t *testing.T) {
	dbPath := newTestDB(t)
	before := readAll(t, dbPath)

	opt := testOptions(t, dbPath)
	opt.EnsureCursorStopped = func() error { return &ErrCursorRunning{PID: 11960} }

	acc := newAccount(t, "auth0|user_NEWACCOUNT", time.Now().Add(72*time.Hour))
	_, err := Switch(context.Background(), acc, opt)
	if err == nil {
		t.Fatal("Cursor 在运行时必须拒绝切号")
	}
	var running *ErrCursorRunning
	if !errors.As(err, &running) {
		t.Fatalf("错误类型应为 ErrCursorRunning，实际 %T", err)
	}
	if !strings.Contains(err.Error(), "11960") || !strings.Contains(err.Error(), "-kill") {
		t.Errorf("提示应包含 PID 与解决办法，实际 %q", err.Error())
	}

	// 被拒绝时库必须一个字节都没动。
	if fmt.Sprint(readAll(t, dbPath)) != fmt.Sprint(before) {
		t.Error("拒绝执行时不应改动数据库")
	}
}

// TestSwitchRefusesExpiredToken 目标 token 已过期时默认拒绝，显式允许才继续。
func TestSwitchRefusesExpiredToken(t *testing.T) {
	dbPath := newTestDB(t)
	expired := newAccount(t, "auth0|user_EXPIRED", time.Now().Add(-time.Hour))

	_, err := Switch(context.Background(), expired, testOptions(t, dbPath))
	if err == nil {
		t.Fatal("过期 token 应默认拒绝")
	}
	var exp *ErrTokenExpired
	if !errors.As(err, &exp) {
		t.Fatalf("错误类型应为 ErrTokenExpired，实际 %T", err)
	}
	if !strings.Contains(err.Error(), "-allow-expired") {
		t.Errorf("提示应给出绕过办法，实际 %q", err.Error())
	}
	if readAll(t, dbPath)[vscdb.KeyCachedEmail].Value != "old@example.com" {
		t.Error("被拒绝时不应改动数据库")
	}

	// 显式允许后应当能写进去。
	opt := testOptions(t, dbPath)
	opt.AllowExpired = true
	if _, err := Switch(context.Background(), expired, opt); err != nil {
		t.Fatalf("显式允许过期后应当成功: %v", err)
	}
	if readAll(t, dbPath)[vscdb.KeyCachedEmail].Value != "new@example.com" {
		t.Error("显式允许后应已写入")
	}
}

// TestSwitchRejectsAccountWithoutItems 账号库里没有原始 key 时要报清楚。
func TestSwitchRejectsAccountWithoutItems(t *testing.T) {
	dbPath := newTestDB(t)

	_, err := Switch(context.Background(), acctstore.Account{UserID: "user_EMPTY"}, testOptions(t, dbPath))
	if err == nil || !strings.Contains(err.Error(), "没有保存原始 key") {
		t.Errorf("应提示缺少原始 key，实际 %v", err)
	}

	partial := acctstore.Account{UserID: "user_PARTIAL", Items: map[string]string{
		vscdb.KeyCachedEmail: "x@example.com",
	}}
	_, err = Switch(context.Background(), partial, testOptions(t, dbPath))
	if err == nil || !strings.Contains(err.Error(), vscdb.KeyAccessToken) {
		t.Errorf("缺 accessToken 应明确报出来，实际 %v", err)
	}
}

// TestCheckpointRuns WAL 模式下 checkpoint 要真的执行且不被阻塞。
func TestCheckpointRuns(t *testing.T) {
	dbPath := newTestDB(t)
	acc := newAccount(t, "auth0|user_NEWACCOUNT", time.Now().Add(72*time.Hour))

	rep, err := Switch(context.Background(), acc, testOptions(t, dbPath))
	if err != nil {
		t.Fatalf("切号失败: %v", err)
	}
	if rep.JournalMode != "wal" {
		t.Errorf("测试库应是 wal 模式，实际 %q", rep.JournalMode)
	}
	if rep.Checkpoint.Busy != 0 {
		t.Errorf("checkpoint 不应被阻塞，busy=%d", rep.Checkpoint.Busy)
	}
	// TRUNCATE 成功后 -wal 应被清空（或已不存在）。
	if st, err := os.Stat(dbPath + "-wal"); err == nil && st.Size() > 0 {
		t.Errorf("checkpoint(TRUNCATE) 后 -wal 应被清空，实际 %d 字节", st.Size())
	}
}

// TestMissingKeysReported 缺失的可写 key 要能列出来供 CLI 提示。
func TestMissingKeysReported(t *testing.T) {
	acc := acctstore.Account{UserID: "user_X", Items: map[string]string{
		vscdb.KeyAccessToken:  "tok",
		vscdb.KeyRefreshToken: "tok",
	}}
	missing := MissingKeys(acc)
	if len(missing) != 4 {
		t.Errorf("应报出 4 个缺失 key，实际 %v", missing)
	}
}

// TestLoadBackupCorrupted 损坏的备份文件要给出可理解的错误。
func TestLoadBackupCorrupted(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	if _, err := LoadBackup(bad); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Errorf("应提示文件损坏，实际 %v", err)
	}

	empty := filepath.Join(dir, "empty.json")
	body, _ := json.Marshal(Backup{})
	if err := os.WriteFile(empty, body, 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	if _, err := LoadBackup(empty); err == nil || !strings.Contains(err.Error(), "没有任何 key") {
		t.Errorf("空备份应报错，实际 %v", err)
	}

	if _, err := LoadBackup(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("不存在的备份应报错")
	}
}
