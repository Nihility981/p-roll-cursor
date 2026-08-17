package vscdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// 本文件是 state.vscdb 的**读写**入口，专供切号使用。
//
// 只读路径（Open / GetItem / LoadAuthState）在 vscdb.go 里原样保留，没有被改动：
// 探测 CLI 仍然走那条路，只读这条边界在代码层面依旧一眼可见。

// SwitchWriteKeys 是切号时需要写入新账号值的 key。
//
// 取舍依据（本机 Cursor 3.15.19 实测）：
//   - 这 6 个都是**随账号变化**的值，留着上一个账号的会造成身份错乱；
//   - Rust 参照实现还会写 cursorAuth/stripeSubscriptionStatus、cursor.accessToken、
//     cursor.email 三个 key，但它们在新版库里根本不存在，Cursor 也从不读，
//     每次切号塞进去只是往库里加垃圾，这里不写；
//   - stripeMembershipAuthId 与 JWT 的 sub 逐字节相同，属于账号身份的一部分，
//     我们手上正好有准确值，所以选择写入而不是删除——删掉会留下一段
//     「有 token 却没有 authId」的中间状态，写入才能让 8 个 key 全部自洽。
var SwitchWriteKeys = []string{
	KeyAccessToken,
	KeyRefreshToken,
	KeyCachedEmail,
	KeyCachedSignUpType,
	KeyStripeMembershipType,
	KeyStripeMembershipAuthID,
}

// SwitchDeleteKeys 是切号时必须删除的 key。
//
// 这两个是**账号绑定的缓存**，不是凭据：只换 token 而留着它们，新账号会继承
// 上一个账号的 profile 缓存（形如 {"displayName":"Example User"}）。
// 我们没有新账号对应的正确值，交给 Cursor 登录后自己重建，所以删除而非写入。
var SwitchDeleteKeys = []string{
	KeyCachedScopedProfile,
	KeyOnboardingDate,
}

// SwitchTouchedKeys 是切号会动到的全部 key，备份旧值时按这个清单来。
func SwitchTouchedKeys() []string {
	keys := make([]string, 0, len(SwitchWriteKeys)+len(SwitchDeleteKeys))
	keys = append(keys, SwitchWriteKeys...)
	keys = append(keys, SwitchDeleteKeys...)
	return keys
}

// RW 是 state.vscdb 的读写句柄。
type RW struct {
	db   *sql.DB
	path string
}

// OpenRW 以**读写**方式打开 state.vscdb。
//
// 与只读的 Open 相比：去掉 mode=ro 与 query_only(1)，其余约束全部保留——
//   - busy_timeout：别的进程短暂持锁时退避，而不是立刻报 SQLITE_BUSY；
//   - SetMaxOpenConns(1)：库有 3.3 GB，多连接只会放大开销与锁竞争；
//   - filepath.ToSlash：Windows 反斜杠在 file: URI 里会被当转义符。
//
// 注意：能拿到写句柄**不代表**现在切号是安全的。实测数据库并没有独占锁，
// Cursor 运行时照样能写进去——但 Cursor 把认证缓存在内存里，退出时会把旧值刷回来，
// 直接盖掉刚写入的账号。所以调用方必须先确认 Cursor 已经退出。
func OpenRW(path string) (*RW, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", filepath.ToSlash(path))

	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 state.vscdb（读写）失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接 state.vscdb（读写）失败: %w", err)
	}

	return &RW{db: sqlDB, path: path}, nil
}

// Close 释放连接。
func (w *RW) Close() error {
	if w == nil || w.db == nil {
		return nil
	}
	return w.db.Close()
}

// Path 返回数据库文件路径。
func (w *RW) Path() string { return w.path }

// GetItem 读取一个 key，语义与只读句柄的 GetItem 一致。
//
// 这里没有复用 DB.GetItem，是为了不改动 vscdb.go 里既有的只读代码——
// 那条路径要保持原样，让「只读」在代码层面继续一目了然。
func (w *RW) GetItem(ctx context.Context, key string) (Item, error) {
	item := Item{Key: key}
	row := w.db.QueryRowContext(ctx,
		"SELECT value, typeof(value) FROM ItemTable WHERE key = ? LIMIT 1", key)

	var value []byte
	var typeOf string
	switch err := row.Scan(&value, &typeOf); {
	case errors.Is(err, sql.ErrNoRows):
		return item, nil
	case err != nil:
		return item, fmt.Errorf("读取 key %q 失败: %w", key, err)
	}

	item.Exists = true
	item.Value = string(value)
	item.TypeOf = typeOf
	return item, nil
}

// Snapshot 读出给定 key 的当前值，用于切号前备份旧值。
func (w *RW) Snapshot(ctx context.Context, keys []string) (map[string]Item, error) {
	out := make(map[string]Item, len(keys))
	for _, key := range keys {
		item, err := w.GetItem(ctx, key)
		if err != nil {
			return nil, err
		}
		out[key] = item
	}
	return out, nil
}

// Apply 在**同一个事务**里写入 sets、删除 deletes。
//
// 关键约束：绑定参数必须是 string，**绝对不能传 []byte**。
// ItemTable 的建表语句是 `value BLOB`（BLOB 亲和性），传 []byte 会静默存成
// blob，typeof(value) 变成 'blob'，Cursor 侧读出来可能是 Buffer 而不是字符串。
// Rust 的 rusqlite 传 &str 天然落成 TEXT，所以那边从未暴露过这个坑。
func (w *RW) Apply(ctx context.Context, sets map[string]string, deletes []string) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO ItemTable(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
	if err != nil {
		return fmt.Errorf("准备写入语句失败: %w", err)
	}
	defer insert.Close()

	for _, key := range sortedKeys(sets) {
		// 第二个参数刻意保持 string 类型，见上面的说明。
		value := sets[key]
		if _, err := insert.ExecContext(ctx, key, value); err != nil {
			return fmt.Errorf("写入 key %q 失败: %w", key, err)
		}
	}

	del, err := tx.PrepareContext(ctx, "DELETE FROM ItemTable WHERE key = ?")
	if err != nil {
		return fmt.Errorf("准备删除语句失败: %w", err)
	}
	defer del.Close()

	for _, key := range deletes {
		if _, err := del.ExecContext(ctx, key); err != nil {
			return fmt.Errorf("删除 key %q 失败: %w", key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// CheckpointResult 是 wal_checkpoint 的返回。
type CheckpointResult struct {
	// Busy 非 0 表示 checkpoint 被其他连接阻塞。
	Busy int64
	// LogFrames 是 WAL 里的总帧数，Checkpointed 是已回写的帧数。
	LogFrames    int64
	Checkpointed int64
}

// Checkpoint 执行 PRAGMA wal_checkpoint(TRUNCATE)。
//
// Cursor 显式开了 WAL（同目录 state.vscdb.options.json 内容为 {"useWAL": true}），
// 不主动 checkpoint 的话，改动会停在 -wal 文件里；TRUNCATE 模式会把 WAL 回写进
// 主库并把它清空，让新账号立刻对下一次启动的 Cursor 可见。
func (w *RW) Checkpoint(ctx context.Context) (CheckpointResult, error) {
	var r CheckpointResult
	row := w.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err := row.Scan(&r.Busy, &r.LogFrames, &r.Checkpointed); err != nil {
		return r, fmt.Errorf("执行 wal_checkpoint 失败: %w", err)
	}
	return r, nil
}

// JournalMode 返回当前 journal 模式。
func (w *RW) JournalMode(ctx context.Context) (string, error) {
	var mode string
	if err := w.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return "", fmt.Errorf("读取 journal_mode 失败: %w", err)
	}
	return mode, nil
}

// TypeOf 返回某个 key 的 typeof(value)，用于验证写入落成了 text 而不是 blob。
func (w *RW) TypeOf(ctx context.Context, key string) (string, error) {
	item, err := w.GetItem(ctx, key)
	if err != nil {
		return "", err
	}
	if !item.Exists {
		return "", fmt.Errorf("key %q 不存在", key)
	}
	return item.TypeOf, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 固定顺序，让写入行为可复现、日志可对照。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
