// Package vscdb 提供对 Cursor state.vscdb 的只读访问。
//
// 当前阶段（PoC）刻意只实现读取：写入/切号会在后续独立文件中加入，
// 以便「只读」这条边界在代码层面一眼可见。
package vscdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// 认证相关的 key。带 ✓ 的是本机 Cursor 3.15.19 实测存在的。
const (
	KeyAccessToken            = "cursorAuth/accessToken"            // ✓ JWT
	KeyRefreshToken           = "cursorAuth/refreshToken"           // ✓ 实测与 accessToken 相同
	KeyCachedEmail            = "cursorAuth/cachedEmail"            // ✓
	KeyStripeMembershipType   = "cursorAuth/stripeMembershipType"   // ✓
	KeyCachedSignUpType       = "cursorAuth/cachedSignUpType"       // ✓
	KeyStripeMembershipAuthID = "cursorAuth/stripeMembershipAuthId" // ✓ auth0|xxx
	KeyCachedScopedProfile    = "cursorAuth/cachedScopedProfile"    // ✓
	KeyOnboardingDate         = "cursorAuth/onboardingDate"         // ✓

	// 以下 key 在新版 Cursor 已不存在，保留常量是为了在探测输出里明确展示「查无此项」，
	// 避免后续实现继续照抄 Rust 版的错误假设。
	KeyStripeSubscriptionStatus = "cursorAuth/stripeSubscriptionStatus" // ✗
	KeyLegacyAuthID             = "cursorAuth/authId"                   // ✗ 真名是 stripeMembershipAuthId
	KeyLegacyAccessToken        = "cursor.accessToken"                  // ✗
	KeyLegacyEmail              = "cursor.email"                        // ✗
)

// AuthKeys 是探测时需要逐个检查的 key 清单，顺序即输出顺序。
var AuthKeys = []string{
	KeyAccessToken,
	KeyRefreshToken,
	KeyCachedEmail,
	KeyStripeMembershipType,
	KeyCachedSignUpType,
	KeyStripeMembershipAuthID,
	KeyCachedScopedProfile,
	KeyOnboardingDate,
	KeyStripeSubscriptionStatus,
	KeyLegacyAuthID,
	KeyLegacyAccessToken,
	KeyLegacyEmail,
}

// DB 是 state.vscdb 的只读句柄。
type DB struct {
	db   *sql.DB
	path string
}

// Open 以只读方式打开 state.vscdb。
//
// DSN 的三个约束都不是可选项：
//   - mode=ro + query_only(1)：双保险，驱动层与 SQLite 层都禁止写入；
//   - 绝不能加 immutable=1：Cursor 显式启用了 WAL（同目录 state.vscdb.options.json
//     内容为 {"useWAL": true}），immutable 会让 SQLite 跳过 -wal 文件，
//     读到的是最后一次 checkpoint 之前的旧数据（本机 WAL 常年有数 MB 未 checkpoint）；
//   - busy_timeout：Cursor 正在运行时写锁可能短暂持有，给 5s 退避。
//
// 另外必须 SetMaxOpenConns(1)：库有 3.4 GB，多连接只会放大打开开销与锁竞争，
// 只读探测串行足够。
func Open(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("未找到 state.vscdb: %s", path)
		}
		return nil, fmt.Errorf("访问 state.vscdb 失败: %w", err)
	}

	dsn := fmt.Sprintf(
		"file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)",
		filepath.ToSlash(path),
	)

	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 state.vscdb 失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接 state.vscdb 失败（编辑器可能正持有独占锁）: %w", err)
	}

	return &DB{db: sqlDB, path: path}, nil
}

// Close 释放连接。
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// Path 返回底层数据库文件路径。
func (d *DB) Path() string { return d.path }

// Item 是 ItemTable 的一行。
type Item struct {
	Key    string
	Value  string
	Exists bool
	// TypeOf 是 SQLite 的 typeof(value)，用于确认 BLOB 亲和性下的真实存储类型。
	TypeOf string
}

// GetItem 读取 ItemTable 中的一个 key。
//
// value 列声明为 BLOB 亲和性，但现存行的 typeof(value) 全是 text。
// 为兼容两种情况，统一扫进 []byte 再转 string——直接扫进 string 在
// 真 BLOB 场景下会带来隐式转换的不确定性。
func (d *DB) GetItem(ctx context.Context, key string) (Item, error) {
	item := Item{Key: key}
	row := d.db.QueryRowContext(ctx,
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

// GetItems 批量读取，缺失的 key 也会返回（Exists=false）。
func (d *DB) GetItems(ctx context.Context, keys []string) ([]Item, error) {
	items := make([]Item, 0, len(keys))
	for _, key := range keys {
		item, err := d.GetItem(ctx, key)
		if err != nil {
			return items, err
		}
		items = append(items, item)
	}
	return items, nil
}

// TableStat 描述一张表的行数。
type TableStat struct {
	Name string
	Rows int64
	Err  error
}

// TableStats 统计给定表的行数。cursorDiskKV 有 28 万行，count 仍在毫秒级，
// 但仍按需调用，不要在热路径上跑。
func (d *DB) TableStats(ctx context.Context, tables []string) []TableStat {
	stats := make([]TableStat, 0, len(tables))
	for _, name := range tables {
		stat := TableStat{Name: name}
		// 表名来自内部常量，不接受外部输入，因此可以安全拼接。
		row := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+name)
		if err := row.Scan(&stat.Rows); err != nil {
			stat.Err = err
		}
		stats = append(stats, stat)
	}
	return stats
}

// JournalMode 返回当前的 journal 模式，用于确认 WAL 生效。
func (d *DB) JournalMode(ctx context.Context) (string, error) {
	var mode string
	if err := d.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return "", fmt.Errorf("读取 journal_mode 失败: %w", err)
	}
	return mode, nil
}

// AuthState 是从 ItemTable 提取出来的登录态快照。
type AuthState struct {
	Items map[string]Item

	AccessToken            string
	RefreshToken           string
	Email                  string
	MembershipType         string
	SignUpType             string
	StripeMembershipAuthID string
	ScopedProfile          string
	OnboardingDate         string
}

// LoadAuthState 一次性读出全部认证相关 key。
func LoadAuthState(ctx context.Context, d *DB) (*AuthState, error) {
	items, err := d.GetItems(ctx, AuthKeys)
	if err != nil {
		return nil, err
	}

	state := &AuthState{Items: make(map[string]Item, len(items))}
	for _, item := range items {
		state.Items[item.Key] = item
	}

	get := func(key string) string { return state.Items[key].Value }
	state.AccessToken = get(KeyAccessToken)
	state.RefreshToken = get(KeyRefreshToken)
	state.Email = get(KeyCachedEmail)
	state.MembershipType = get(KeyStripeMembershipType)
	state.SignUpType = get(KeyCachedSignUpType)
	state.StripeMembershipAuthID = get(KeyStripeMembershipAuthID)
	state.ScopedProfile = get(KeyCachedScopedProfile)
	state.OnboardingDate = get(KeyOnboardingDate)
	return state, nil
}
