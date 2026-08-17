// Package acctstore 实现账号库：把从 Cursor 读出的登录态存成我们自己的文件，
// 供以后切号时写回。
//
// # 关于「全程只读」这条底线
//
// README 里的「全程只读」指的是**不动 Cursor 的任何文件**——不写、不改、不删
// state.vscdb 及其 -wal / -shm。本包会写文件，但写的是**我们自己的账号库**，
// 位置在 Cursor 数据目录之外，与 Cursor 毫无关系。这**不违反**那条底线。
// 请不要因为看到本包在写文件，就以为只读边界已经被打破了。
//
// # 账号库为什么不放进仓库
//
// 账号记录里存着 refresh token，等同于账号密码。放进仓库早晚会被 git add，
// 所以默认落在用户目录（%APPDATA%\cursor-switch\），物理上位于仓库之外，
// 想误提交都提交不了。
package acctstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// EnvStorePath 可以覆盖账号库位置，便于测试与多套配置并存。
const EnvStorePath = "CURSOR_SWITCH_STORE"

// FormatVersion 是账号库文件的格式版本，将来结构变化时用于迁移。
const FormatVersion = 1

// storePerm 是创建账号库文件时传入的权限位。
//
// 重要：**在 Windows 上这个值基本不起作用。** Go 不会把它翻译成 ACL，
// 新文件的实际权限完全继承自父目录。本机实测 %APPDATA% 上就显式挂着一条
// CodexSandboxUsers 组的读权限 ACE 并向下继承，所以账号库的真实可访问者是
// 「当前用户 + SYSTEM + Administrators + 该组可读」，并不是 0600 字面上的独占。
//
// 换句话说：这里**没有任何强制访问控制**，不要把它当成安全边界。
// 目前的实质保护只有两点——文件位于用户 profile 内，以及它在仓库之外。
// 真要收紧，得显式设置 ACL，或者把内容加密（Codec 接口已经为此留好位置）。
// 作为参照，Cursor 自己的 state.vscdb 在同一台机器上暴露面更大（同组可 Modify）。
const storePerm = 0o600

// dirPerm 同理，Windows 下不生效，仅对类 Unix 系统有意义。
const dirPerm = 0o700

// Account 是账号库里的一条记录。
type Account struct {
	// UserID 是去重主键：JWT sub 按 '|' 切最后一段，形如 user_01KY...。
	// 刻意不用邮箱做主键——邮箱可以改，用户 ID 不会变。
	UserID string `json:"userId"`
	Email  string `json:"email,omitempty"`
	// MembershipType 是套餐，例如 enterprise / pro / free。
	MembershipType string `json:"membershipType,omitempty"`
	// SignUpType 是注册方式，例如 Auth_0。
	SignUpType string `json:"signUpType,omitempty"`
	// Sub 是 JWT 里未经切分的原始 sub，形如 auth0|user_01KY...。
	// 实测它与 cursorAuth/stripeMembershipAuthId 逐字节相同，不是两个独立信息源。
	Sub string `json:"sub,omitempty"`
	// TokenExpiresAt 来自 JWT 的 exp。账号库里堆多个账号后，
	// 靠它一眼看出哪些 token 已经过期，不用挨个去试。
	TokenExpiresAt *time.Time `json:"tokenExpiresAt,omitempty"`
	// ExportedAt 是这条记录的导出时间。
	ExportedAt time.Time `json:"exportedAt"`
	// Items 是 state.vscdb 里认证相关 key 的**原始全量快照**。
	// 切号时到底要写回哪几个 key 尚未定，先全部留着，信息不要丢。
	Items map[string]string `json:"items"`
}

// IsExpired 判断 token 在给定时刻是否已过期。
// 没有 exp 时返回 false——「不知道」不等于「已过期」。
func (a Account) IsExpired(now time.Time) bool {
	if a.TokenExpiresAt == nil {
		return false
	}
	// exp 那一秒本身仍算有效，与 JWT 惯例一致：过期是 now > exp。
	return now.After(*a.TokenExpiresAt)
}

// ExpiresIn 返回距离过期还有多久；没有 exp 时返回 0 和 false。
func (a Account) ExpiresIn(now time.Time) (time.Duration, bool) {
	if a.TokenExpiresAt == nil {
		return 0, false
	}
	return a.TokenExpiresAt.Sub(now), true
}

// File 是账号库文件的顶层结构。
type File struct {
	Version int `json:"version"`
	// Encryption 记录载荷的加密方式，当前恒为 "none"。
	// 留这个字段是为了将来换成加密实现时，老文件能被正确识别。
	Encryption string    `json:"encryption"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Accounts   []Account `json:"accounts"`
}

// Find 按 userID 查记录。
func (f File) Find(userID string) (Account, int, bool) {
	for i, a := range f.Accounts {
		if a.UserID == userID {
			return a, i, true
		}
	}
	return Account{}, -1, false
}

// Codec 负责账号库在「内存结构」与「落盘字节」之间的转换。
//
// 抽成接口是为了将来加密时只需换一个实现——调用方与 Account/File 结构
// 都不用改。当前用明文 JSON，与「取消全部脱敏」的既定策略一致，也便于排查。
type Codec interface {
	// Name 是写进文件 Encryption 字段的标识。
	Name() string
	Encode(File) ([]byte, error)
	Decode([]byte) (File, error)
}

// JSONCodec 是明文 JSON 实现。
type JSONCodec struct{}

func (JSONCodec) Name() string { return "none" }

func (JSONCodec) Encode(f File) ([]byte, error) {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化账号库失败: %w", err)
	}
	return append(data, '\n'), nil
}

func (JSONCodec) Decode(data []byte) (File, error) {
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("账号库 JSON 解析失败（文件可能已损坏）: %w", err)
	}
	return f, nil
}

// Store 是账号库的读写入口。
type Store struct {
	path  string
	codec Codec
}

// New 用指定路径与编解码器创建 Store。
func New(path string, codec Codec) *Store {
	if codec == nil {
		codec = JSONCodec{}
	}
	return &Store{path: path, codec: codec}
}

// Open 用默认路径（可被环境变量覆盖）创建 Store。
func Open(override string) (*Store, error) {
	path, err := ResolvePath(override)
	if err != nil {
		return nil, err
	}
	return New(path, JSONCodec{}), nil
}

// Path 返回账号库文件路径。
func (s *Store) Path() string { return s.path }

// ResolvePath 决定账号库位置，优先级：
// 显式参数 > 环境变量 CURSOR_SWITCH_STORE > 用户配置目录下的 cursor-switch/accounts.json。
func ResolvePath(override string) (string, error) {
	if override != "" {
		return filepath.Clean(override), nil
	}
	if env := os.Getenv(EnvStorePath); env != "" {
		return filepath.Clean(env), nil
	}
	// Windows 下 UserConfigDir 就是 %APPDATA%。
	dir, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", fmt.Errorf("无法定位账号库目录: %w", err)
		}
		dir = filepath.Join(home, ".cursor-switch")
		return filepath.Join(dir, "accounts.json"), nil
	}
	return filepath.Join(dir, "cursor-switch", "accounts.json"), nil
}

// Load 读出账号库。文件不存在视为空库，不算错误——首次使用是正常情况。
func (s *Store) Load() (File, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return File{Version: FormatVersion, Encryption: s.codec.Name()}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("读取账号库 %s 失败: %w", s.path, err)
	}
	if len(data) == 0 {
		return File{}, fmt.Errorf("账号库 %s 是空文件，可能上次写入被中断；请检查后手工删除或恢复", s.path)
	}

	f, err := s.codec.Decode(data)
	if err != nil {
		return File{}, fmt.Errorf("账号库 %s 无法解析: %w", s.path, err)
	}
	if f.Version > FormatVersion {
		return File{}, fmt.Errorf("账号库 %s 的格式版本是 %d，高于本程序支持的 %d，请升级工具",
			s.path, f.Version, FormatVersion)
	}
	return f, nil
}

// Save 原子地写回账号库。
func (s *Store) Save(f File) error {
	f.Version = FormatVersion
	f.Encryption = s.codec.Name()
	f.UpdatedAt = time.Now()
	sort.Slice(f.Accounts, func(i, j int) bool { return f.Accounts[i].UserID < f.Accounts[j].UserID })

	data, err := s.codec.Encode(f)
	if err != nil {
		return err
	}
	return writeAtomic(s.path, data)
}

// renameFile 抽成变量是为了让测试能模拟「临时文件已写好、替换却失败」这一步，
// 从而验证失败时原账号库不会被破坏。生产路径上它就是 os.Rename。
var renameFile = os.Rename

// writeAtomic 先写同目录下的临时文件，再整体重命名覆盖。
//
// 不能就地改写：这是存凭据的文件，写到一半崩了会把整个账号库毁掉，
// 而 refresh token 丢了就得重新登录每一个账号。临时文件必须和目标同目录，
// 否则跨卷时 rename 不是原子操作。
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("创建账号库目录 %s 失败: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	// 任何中途失败都要清理临时文件，别在用户目录里留垃圾。
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	// Sync 之后再 rename，确保断电时不会出现「文件名换了但内容还没落盘」。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("刷盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, storePerm); err != nil {
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if err := renameFile(tmpName, path); err != nil {
		return fmt.Errorf("替换账号库 %s 失败（原文件未被改动）: %w", path, err)
	}
	committed = true
	return nil
}

// ConflictPolicy 决定导出时遇到同一 userID 已存在该怎么办。
type ConflictPolicy int

const (
	// ConflictReject 是默认策略：拒绝并报告，由使用者决定是否覆盖。
	//
	// 之所以默认拒绝而不是直接覆盖：账号库目前是这些 refresh token 的唯一副本，
	// 而「同一个 userID 再导出一次」既可能是想更新过期 token（该覆盖），
	// 也可能是误操作（不该覆盖）。程序分辨不出来，所以把决定权交回给人，
	// 并在报错时把新旧两条记录的时间与过期状态都打出来供比较。
	ConflictReject ConflictPolicy = iota
	// ConflictOverwrite 覆盖同 userID 的旧记录。
	ConflictOverwrite
)

// UpsertResult 说明一次导出的结果。
type UpsertResult struct {
	Created bool
	Updated bool
	// Previous 是被覆盖掉的旧记录，仅 Updated 为 true 时有意义。
	Previous Account
	Total    int
}

// ErrDuplicate 表示同一 userID 已存在且策略为拒绝。
var ErrDuplicate = errors.New("账号已存在")

// Upsert 把一条账号记录写进账号库。
func (s *Store) Upsert(acc Account, policy ConflictPolicy) (UpsertResult, error) {
	if acc.UserID == "" {
		return UpsertResult{}, errors.New("账号记录缺少 userId，无法入库")
	}

	f, err := s.Load()
	if err != nil {
		return UpsertResult{}, err
	}

	var res UpsertResult
	if prev, idx, found := f.Find(acc.UserID); found {
		if policy == ConflictReject {
			return UpsertResult{}, fmt.Errorf("%w：userId=%s（已有记录导出于 %s，token 过期时间 %s）",
				ErrDuplicate, acc.UserID,
				prev.ExportedAt.Format(time.RFC3339), formatExpiry(prev.TokenExpiresAt))
		}
		f.Accounts[idx] = acc
		res.Updated = true
		res.Previous = prev
	} else {
		f.Accounts = append(f.Accounts, acc)
		res.Created = true
	}

	if err := s.Save(f); err != nil {
		return UpsertResult{}, err
	}
	res.Total = len(f.Accounts)
	return res, nil
}

func formatExpiry(t *time.Time) string {
	if t == nil {
		return "未知"
	}
	return t.Format(time.RFC3339)
}
