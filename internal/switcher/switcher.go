// Package switcher 实现写入式账号切换。
//
// 完整流程（顺序不能变）：
//
//  1. 确认 Cursor 已退出
//  2. 备份将要改动的 key 的旧值（小 JSON，不是整库）
//  3. 在一个事务里写入新账号的 key、删除账号绑定缓存
//  4. PRAGMA wal_checkpoint(TRUNCATE)
//  5. （由调用方决定）重新启动 Cursor
//
// 第 1 步为什么不能省：**原因不是文件锁**。实测数据库没有独占锁，Cursor 运行时
// 别的进程照样拿得到写句柄、照样写得进去。真正的原因在应用层——Cursor 把认证
// 缓存在内存里，退出时会把旧值刷回数据库，把刚写入的新账号直接盖掉。
package switcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Nihility981/p-roll-cursor/internal/acctstore"
	"github.com/Nihility981/p-roll-cursor/internal/vscdb"
)

// ErrCursorRunning 表示 Cursor 仍在运行，此时切号会被退出时的内存刷回覆盖。
type ErrCursorRunning struct {
	PID int
}

func (e *ErrCursorRunning) Error() string {
	return fmt.Sprintf("编辑器仍在运行（主进程 PID %d）。"+
		"编辑器退出时会把内存里的旧登录态刷回数据库，现在切号会被直接覆盖。\n"+
		"       请先完全退出编辑器再重试，或加 -kill 让本工具替你关闭（会先尝试优雅关闭）", e.PID)
}

// ErrTokenExpired 表示目标账号的 token 已过期。
type ErrTokenExpired struct {
	UserID    string
	ExpiredAt time.Time
}

func (e *ErrTokenExpired) Error() string {
	return fmt.Sprintf("账号 %s 的 token 已于 %s 过期，切过去也是未登录状态。"+
		"确实要写入请加 -allow-expired",
		e.UserID, e.ExpiredAt.Local().Format("2006-01-02 15:04:05"))
}

// Backup 是切号前的旧值存档。
//
// 刻意不做整库备份：库有 3.3 GB，复制一次要好几秒还白占空间，
// 而真正会被改动的只有那 8 个 key，存成一个几 KB 的 JSON 就够回滚了。
type Backup struct {
	CreatedAt time.Time `json:"createdAt"`
	// DBPath 是被改动的 state.vscdb 路径。
	DBPath string `json:"dbPath"`
	// SwitchedTo 是这次切换的目标账号，便于事后辨认这份备份属于哪一步。
	SwitchedTo string `json:"switchedTo"`
	// Items 是旧值。value 为 nil 表示该 key 当时**不存在**——
	// 回滚时要把它删掉，而不是写一个空字符串进去，这两者不是一回事。
	Items map[string]*string `json:"items"`
}

// Options 控制一次切号。
type Options struct {
	// DBPath 是目标 state.vscdb。
	DBPath string
	// BackupDir 是旧值存档目录。
	BackupDir string
	// AllowExpired 为 true 时即使目标 token 已过期也继续。
	AllowExpired bool
	// Now 便于测试注入时间。
	Now time.Time
	// EnsureCursorStopped 检查 Cursor 是否已退出；返回错误则中止。
	// 传 nil 表示跳过检查（仅用于测试）。
	EnsureCursorStopped func() error
}

// Report 是一次切号的结果。
type Report struct {
	Account      acctstore.Account
	BackupPath   string
	WrittenKeys  []string
	DeletedKeys  []string
	Checkpoint   vscdb.CheckpointResult
	JournalMode  string
	VerifiedText []string
}

// Switch 执行一次完整的写入式切号。
func Switch(ctx context.Context, acc acctstore.Account, opt Options) (Report, error) {
	var rep Report
	rep.Account = acc

	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}

	// 1. Cursor 必须已经退出。
	if opt.EnsureCursorStopped != nil {
		if err := opt.EnsureCursorStopped(); err != nil {
			return rep, err
		}
	}

	if !opt.AllowExpired && acc.IsExpired(now) {
		return rep, &ErrTokenExpired{UserID: acc.UserID, ExpiredAt: *acc.TokenExpiresAt}
	}

	sets, err := buildWriteSet(acc)
	if err != nil {
		return rep, err
	}

	db, err := vscdb.OpenRW(opt.DBPath)
	if err != nil {
		return rep, err
	}
	defer db.Close()

	// 2. 备份旧值。备份失败就不往下走——没有退路的写入不能做。
	backupPath, err := writeBackup(ctx, db, opt.BackupDir, acc.UserID, now)
	if err != nil {
		return rep, err
	}
	rep.BackupPath = backupPath

	// 3. 写入 + 删除，同一个事务。
	if err := db.Apply(ctx, sets, vscdb.SwitchDeleteKeys); err != nil {
		return rep, fmt.Errorf("%w\n       旧值已备份在 %s，可用 rollback 恢复", err, backupPath)
	}
	rep.WrittenKeys = sortedKeys(sets)
	rep.DeletedKeys = append([]string{}, vscdb.SwitchDeleteKeys...)

	// 4. 把 WAL 回写进主库，让新账号对下一次启动的 Cursor 立即可见。
	if rep.JournalMode, err = db.JournalMode(ctx); err != nil {
		return rep, err
	}
	if rep.Checkpoint, err = db.Checkpoint(ctx); err != nil {
		return rep, err
	}

	// 写完顺手确认落成的是 text 而不是 blob——这是最容易静默出错的一步。
	for _, key := range rep.WrittenKeys {
		typ, err := db.TypeOf(ctx, key)
		if err != nil {
			return rep, err
		}
		if typ != "text" {
			return rep, fmt.Errorf("key %q 写入后 typeof(value) 是 %q 而不是 text，"+
				"编辑器侧可能读成 Buffer；旧值备份在 %s", key, typ, backupPath)
		}
		rep.VerifiedText = append(rep.VerifiedText, key)
	}

	return rep, nil
}

// buildWriteSet 从账号记录里取出要写入的 key/value。
func buildWriteSet(acc acctstore.Account) (map[string]string, error) {
	if len(acc.Items) == 0 {
		return nil, fmt.Errorf("账号 %s 没有保存原始 key，无法切号（请重新 save 或 import）", acc.UserID)
	}

	sets := make(map[string]string, len(vscdb.SwitchWriteKeys))
	var missing []string
	for _, key := range vscdb.SwitchWriteKeys {
		v, ok := acc.Items[key]
		if !ok || v == "" {
			missing = append(missing, key)
			continue
		}
		sets[key] = v
	}

	// accessToken 缺了就没得切；其余缺失只是降级，不阻断。
	if _, ok := sets[vscdb.KeyAccessToken]; !ok {
		return nil, fmt.Errorf("账号 %s 缺少 %s，无法切号", acc.UserID, vscdb.KeyAccessToken)
	}
	_ = missing
	return sets, nil
}

// MissingKeys 返回账号记录里缺失的可写 key，供 CLI 提示用。
func MissingKeys(acc acctstore.Account) []string {
	var missing []string
	for _, key := range vscdb.SwitchWriteKeys {
		if v, ok := acc.Items[key]; !ok || v == "" {
			missing = append(missing, key)
		}
	}
	return missing
}

// writeBackup 把将要改动的 key 的旧值存成带时间戳的小 JSON。
func writeBackup(ctx context.Context, db *vscdb.RW, dir, target string, now time.Time) (string, error) {
	snapshot, err := db.Snapshot(ctx, vscdb.SwitchTouchedKeys())
	if err != nil {
		return "", fmt.Errorf("备份旧值失败: %w", err)
	}

	backup := Backup{
		CreatedAt:  now,
		DBPath:     db.Path(),
		SwitchedTo: target,
		Items:      make(map[string]*string, len(snapshot)),
	}
	for key, item := range snapshot {
		if !item.Exists {
			backup.Items[key] = nil
			continue
		}
		v := item.Value
		backup.Items[key] = &v
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建备份目录 %s 失败: %w", dir, err)
	}
	name := fmt.Sprintf("switch-%s.json", now.Format("20060102-150405"))
	path := filepath.Join(dir, name)

	data, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化备份失败: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("写入备份 %s 失败: %w", path, err)
	}
	return path, nil
}

// LoadBackup 读回一份旧值存档。
func LoadBackup(path string) (Backup, error) {
	var b Backup
	data, err := os.ReadFile(path)
	if err != nil {
		return b, fmt.Errorf("读取备份 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("解析备份 %s 失败（文件可能损坏）: %w", path, err)
	}
	if len(b.Items) == 0 {
		return b, fmt.Errorf("备份 %s 里没有任何 key", path)
	}
	return b, nil
}

// RollbackReport 是一次回滚的结果。
type RollbackReport struct {
	Restored   []string
	Removed    []string
	Checkpoint vscdb.CheckpointResult
}

// Rollback 用一份旧值存档还原 state.vscdb。
//
// 备份里 value 为 nil 的 key 表示当时**不存在**，回滚时要删掉它，
// 而不是写一个空字符串——「没有这个 key」和「值是空串」不是一回事。
func Rollback(ctx context.Context, b Backup, dbPath string, ensureStopped func() error) (RollbackReport, error) {
	var rep RollbackReport

	if ensureStopped != nil {
		if err := ensureStopped(); err != nil {
			return rep, err
		}
	}

	db, err := vscdb.OpenRW(dbPath)
	if err != nil {
		return rep, err
	}
	defer db.Close()

	sets := map[string]string{}
	var deletes []string
	for key, v := range b.Items {
		if v == nil {
			deletes = append(deletes, key)
			continue
		}
		sets[key] = *v
	}
	sort.Strings(deletes)

	if err := db.Apply(ctx, sets, deletes); err != nil {
		return rep, err
	}
	rep.Restored = sortedKeys(sets)
	rep.Removed = deletes

	if rep.Checkpoint, err = db.Checkpoint(ctx); err != nil {
		return rep, err
	}
	return rep, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
