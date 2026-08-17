// Command acct 是账号切换 CLI：保存、导入、列出账号，并执行写入式切号。
//
// 边界说明：
//   - save / list / import 对 Cursor **只读**（save 只以只读方式打开 state.vscdb）；
//   - switch / at / rollback 会**写入** state.vscdb，这是它们的本职工作。写之前必须
//     确认 Cursor 已退出，否则 Cursor 退出时会把内存里的旧登录态刷回来覆盖掉。
//   - 账号库与备份都写在用户目录（默认 %APPDATA%\cursor-switch\），在仓库之外。
//
// cmd/probe 保持纯只读，不受本命令影响。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Nihility981/p-roll-cursor/internal/acctstore"
	"github.com/Nihility981/p-roll-cursor/internal/paths"
	"github.com/Nihility981/p-roll-cursor/internal/procutil"
	"github.com/Nihility981/p-roll-cursor/internal/switcher"
	"github.com/Nihility981/p-roll-cursor/internal/vscdb"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "save", "export":
		err = runSave(os.Args[2:])
	case "list":
		err = runList(os.Args[2:])
	case "import":
		err = runImport(os.Args[2:])
	case "switch":
		err = runSwitch(os.Args[2:])
	case "at":
		err = runAt(os.Args[2:])
	case "rollback":
		err = runRollback(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "未知子命令：%s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\n[错误] %v\n", err)
		os.Exit(1)
	}
}

const usageText = `acct —— 账号切换

最快的用法（一条命令切号）：
  acct at
  acct at <accessToken>
        导入 → 切号 → 重启编辑器

  acct switch -token "<accessToken>" [-email <邮箱>] [-membership <套餐>]
        从 token 构造账号 → 存进账号库 → 写库切号，不用先单独跑 import
        **执行前必须完全退出编辑器**

  acct switch <邮箱或用户ID> -kill -start
        从账号库切号：关进程 → 切号 → 再拉起

全部子命令：
  acct save   [-store <路径>] [-force]
        把当前登录的账号从 state.vscdb 存进账号库（对本机登录态只读）

  acct import -token <accessToken> [-email <邮箱>] [-membership <套餐>]
              [-signup-type <类型>] [-store <路径>] [-force]
  acct import -file <账号JSON> [-store <路径>] [-force]
        从外部导入一个账号。账号库里只有当前账号的话，切号无从切起，用这个加账号

  acct list   [-store <路径>]
        列出账号库里的账号（邮箱、用户 ID、套餐、token 是否过期）

  acct at     [<accessToken>]
        粘贴或传入 accessToken：导入 → 切号 → 重启编辑器
        等价于 acct switch -token <accessToken> -kill -start

  acct switch <邮箱或用户ID> [-kill] [-start] [-allow-expired]
              [-store <路径>] [-dry-run]
  acct switch -token <accessToken> [-email <邮箱>] [-membership <套餐>]
              [-signup-type <类型>] [其余同上]
        写入式切号。目标二选一：位置参数从账号库里找已有账号，-token 直接用新 token。
        **必须先完全退出编辑器**，否则会被退出时的回写覆盖

  acct rollback <备份文件> [-kill]
        用切号前的旧值存档还原 state.vscdb

注意：账号库与备份里都是明文 refresh token，等同于账号密码，不要提交、不要外发。
`

func usage() {
	fmt.Print(usageText)
}

// ---------------------------------------------------------------------------
// save / import / list
// ---------------------------------------------------------------------------

func runSave(args []string) error {
	fs := flag.NewFlagSet("save", flag.ExitOnError)
	storePath := fs.String("store", "", "账号库文件路径")
	force := fs.Bool("force", false, "同一用户 ID 已存在时覆盖")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cursorPaths, err := paths.Resolve()
	if err != nil {
		return err
	}

	// 只读打开，save 不会写 Cursor 的任何文件。
	db, err := vscdb.Open(cursorPaths.StateDB)
	if err != nil {
		return err
	}
	defer db.Close()

	state, err := vscdb.LoadAuthState(ctx, db)
	if err != nil {
		return err
	}
	acc, err := acctstore.FromAuthState(state, time.Now())
	if err != nil {
		return err
	}

	fmt.Printf("来源 state.vscdb : %s（只读）\n", db.Path())
	return storeAccount(acc, *storePath, *force)
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	storePath := fs.String("store", "", "账号库文件路径")
	force := fs.Bool("force", false, "同一用户 ID 已存在时覆盖")
	token := fs.String("token", "", "accessToken（JWT）")
	file := fs.String("file", "", "账号 JSON 文件")
	email := fs.String("email", "", "邮箱（-token 模式下可选）")
	membership := fs.String("membership", "", "套餐，如 pro / enterprise / free")
	signUpType := fs.String("signup-type", "Auth_0", "注册方式")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var items map[string]string
	switch {
	case *file != "" && *token != "":
		return errors.New("-file 与 -token 只能二选一")
	case *file != "":
		var err error
		if items, err = readImportFile(*file); err != nil {
			return err
		}
	case *token != "":
		items = itemsFromToken(strings.TrimSpace(*token), *email, *membership, *signUpType)
	default:
		return errors.New("请用 -token 提供 accessToken，或用 -file 指定账号 JSON")
	}

	acc, err := acctstore.FromItems(items, time.Now())
	if err != nil {
		return err
	}
	fmt.Println("来源             : 外部导入")
	return storeAccount(acc, *storePath, *force)
}

// accountFromToken 把一个 accessToken 变成账号记录。
//
// `import -token` 与 `switch -token` 共用这一段，两条路径必须解析出完全相同的
// 账号，否则「先 import 再 switch」和「一条命令直切」就会得到不同结果。
func accountFromToken(token, email, membership, signUpType string, now time.Time) (acctstore.Account, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return acctstore.Account{}, errors.New("-token 的值为空")
	}
	return acctstore.FromItems(itemsFromToken(token, email, membership, signUpType), now)
}

// itemsFromToken 只有一个 accessToken 时，尽量把能推出来的 key 都补齐。
//
// refreshToken 直接复用 accessToken：本机实测这两个 key 的值逐字节相同。
// stripeMembershipAuthId 用 JWT 的 sub：实测两者也是逐字节相同，不是独立信息源。
func itemsFromToken(token, email, membership, signUpType string) map[string]string {
	items := map[string]string{
		vscdb.KeyAccessToken:  token,
		vscdb.KeyRefreshToken: token,
	}
	if sub := subFromToken(token); sub != "" {
		items[vscdb.KeyStripeMembershipAuthID] = sub
	}
	if email != "" {
		items[vscdb.KeyCachedEmail] = email
	}
	if membership != "" {
		items[vscdb.KeyStripeMembershipType] = membership
	}
	if signUpType != "" {
		items[vscdb.KeyCachedSignUpType] = signUpType
	}
	return items
}

func subFromToken(token string) string {
	acc, err := acctstore.FromItems(map[string]string{vscdb.KeyAccessToken: token}, time.Now())
	if err != nil {
		return ""
	}
	return acc.Sub
}

// readImportFile 接受两种形态：{"items":{...}} 或直接就是 key/value 平铺的对象。
func readImportFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	var wrapper struct {
		Items map[string]string `json:"items"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Items) > 0 {
		return wrapper.Items, nil
	}

	var flat map[string]string
	if err := json.Unmarshal(data, &flat); err != nil {
		return nil, fmt.Errorf("解析 %s 失败：期望是 {\"items\":{...}} 或 key/value 平铺的对象: %w", path, err)
	}
	if len(flat) == 0 {
		return nil, fmt.Errorf("%s 里没有任何 key", path)
	}
	return flat, nil
}

func storeAccount(acc acctstore.Account, storePath string, force bool) error {
	store, err := acctstore.Open(storePath)
	if err != nil {
		return err
	}
	fmt.Printf("账号库           : %s\n", store.Path())
	printAccount(acc, time.Now())

	if !acctstore.UserIDLooksStandard(acc.UserID) {
		fmt.Printf("\n[注意] 用户 ID %q 不是预期的 user_ 前缀，请人工确认切分是否正确。\n", acc.UserID)
	}
	if missing := switcher.MissingKeys(acc); len(missing) > 0 {
		fmt.Printf("\n[注意] 以下切号需要的 key 缺失，切过去后这些字段会保留旧账号的值：\n       %s\n",
			strings.Join(missing, ", "))
	}

	policy := acctstore.ConflictReject
	if force {
		policy = acctstore.ConflictOverwrite
	}
	res, err := store.Upsert(acc, policy)
	if err != nil {
		if errors.Is(err, acctstore.ErrDuplicate) {
			return fmt.Errorf("%w\n       如需覆盖旧记录请加 -force 重跑", err)
		}
		return err
	}

	switch {
	case res.Created:
		fmt.Printf("\n已新增账号，账号库现有 %d 个账号。\n", res.Total)
	case res.Updated:
		fmt.Printf("\n已覆盖同一用户 ID 的旧记录（原记录导出于 %s），账号库现有 %d 个账号。\n",
			res.Previous.ExportedAt.Local().Format("2006-01-02 15:04:05"), res.Total)
	}
	fmt.Println("提醒：账号库内是明文凭据，注意不要外传或提交到版本库。")
	return nil
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	storePath := fs.String("store", "", "账号库文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := acctstore.Open(*storePath)
	if err != nil {
		return err
	}
	f, err := store.Load()
	if err != nil {
		return err
	}

	fmt.Printf("账号库 : %s\n", store.Path())
	fmt.Printf("格式   : v%d，加密方式 %s\n", f.Version, orNone(f.Encryption))
	if len(f.Accounts) == 0 {
		fmt.Println("\n账号库为空。先跑 `acct save` 存下当前账号，再用 `acct import` 加入其他账号。")
		return nil
	}

	now := time.Now()
	fmt.Printf("\n共 %d 个账号：\n", len(f.Accounts))
	for i, a := range f.Accounts {
		fmt.Printf("\n  [%d] %s\n", i+1, orDash(a.Email))
		fmt.Printf("      用户 ID    : %s\n", a.UserID)
		fmt.Printf("      套餐       : %s\n", orDash(a.MembershipType))
		fmt.Printf("      注册方式   : %s\n", orDash(a.SignUpType))
		fmt.Printf("      token 状态 : %s\n", expiryText(a, now))
		fmt.Printf("      导出于     : %s\n", a.ExportedAt.Local().Format("2006-01-02 15:04:05"))
		fmt.Printf("      原始 key   : %d 个\n", len(a.Items))
	}
	fmt.Println("\n切号：acct switch <邮箱或用户ID>（先完全退出编辑器）")
	return nil
}

// ---------------------------------------------------------------------------
// at / switch / rollback
// ---------------------------------------------------------------------------

// runAt 是一键切号：读到 accessToken 后走与
// `acct switch -token <token> -kill -start` 完全相同的路径。
func runAt(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Print(atUsageText)
		return nil
	}
	token, extra, err := resolveAtToken(args, os.Stdin)
	if err != nil {
		return err
	}
	return runSwitch(atSwitchArgs(token, extra))
}

const atUsageText = `用法：
  acct at
  acct at <accessToken>
        导入 → 切号 → 重启编辑器
        等价于 acct switch -token <accessToken> -kill -start
`

// atSwitchArgs 构造与 `switch -token <token> -kill -start` 等价的参数。
// extra 原样追加，便于测试注入 -store / -db / -dry-run，不会改默认的关进程与拉起。
func atSwitchArgs(token string, extra []string) []string {
	out := []string{"-token", token, "-kill", "-start"}
	return append(out, extra...)
}

func resolveAtToken(args []string, in io.Reader) (string, []string, error) {
	token, extra := splitPositional(args, switchValueFlags)
	if token != "" {
		token = strings.TrimSpace(token)
		if token == "" {
			return "", extra, errors.New("token 为空")
		}
		return token, extra, nil
	}
	token, err := readAtToken(in)
	if err != nil {
		return "", extra, err
	}
	if token == "" {
		return "", extra, errors.New("token 为空")
	}
	return token, extra, nil
}

func readAtToken(in io.Reader) (string, error) {
	fmt.Fprintln(os.Stderr, "请粘贴 accessToken 后回车")
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", errors.New("token 为空")
	}
	return strings.TrimSpace(sc.Text()), nil
}

func runSwitch(args []string) error {
	fs := flag.NewFlagSet("switch", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print(`用法：
  acct switch <邮箱或用户ID> [-kill] [-start] [-allow-expired] [-store <路径>] [-dry-run]
  acct switch -token <accessToken> [-email <邮箱>] [-membership <套餐>] [-signup-type <类型>] [其余同上]

最快的用法：
  acct at
  acct at <accessToken>
        导入 → 切号 → 重启编辑器
  acct switch -token "<accessToken>"
        从 token 构造账号 → 存进账号库 → 写库切号
  acct switch <邮箱或用户ID> -kill -start
        从账号库切号：关进程 → 切号 → 再拉起

参数：
`)
		fs.PrintDefaults()
	}
	storePath := fs.String("store", "", "账号库文件路径")
	killCursor := fs.Bool("kill", false, "编辑器还在跑时先优雅关闭（超时才强杀）")
	startCursor := fs.Bool("start", false, "切号完成后重新启动")
	allowExpired := fs.Bool("allow-expired", false, "即使目标 token 已过期也继续")
	dryRun := fs.Bool("dry-run", false, "只显示将要做什么，不写入")
	dbOverride := fs.String("db", "", "指定另一个 state.vscdb（仅用于验证，默认用本机登录态的）")
	token := fs.String("token", "", "直接用 accessToken 切号，省掉先跑 import 的一步")
	email := fs.String("email", "", "配合 -token：邮箱")
	membership := fs.String("membership", "", "配合 -token：套餐")
	signUpType := fs.String("signup-type", "Auth_0", "配合 -token：注册方式")

	target, rest := splitPositional(args, switchValueFlags)
	if err := fs.Parse(rest); err != nil {
		return err
	}

	// 目标二选一：库里已有的账号（位置参数），或直接给一个 token。
	hasToken := strings.TrimSpace(*token) != ""
	switch {
	case target == "" && !hasToken:
		return errors.New("请指定要切换到的账号，两种用法：\n" +
			"       acct switch <邮箱或用户ID>        从账号库里已有的账号切\n" +
			"       acct switch -token <accessToken>  直接用 token 切（会顺便存进账号库）")
	case target != "" && hasToken:
		return fmt.Errorf("位置参数 %q 与 -token 只能二选一："+
			"前者是从账号库里找已有账号，后者是直接用新 token，同时给出无法判断你要哪个", target)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	store, err := acctstore.Open(*storePath)
	if err != nil {
		return err
	}

	var acc acctstore.Account
	if hasToken {
		// 复用 import 那条构造逻辑，不另写一份。
		if acc, err = accountFromToken(*token, *email, *membership, *signUpType, time.Now()); err != nil {
			return err
		}
		fmt.Println("来源       : -token 直接构造（切号前会存进账号库）")
	} else {
		f, loadErr := store.Load()
		if loadErr != nil {
			return loadErr
		}
		if acc, err = resolveAccount(f, target); err != nil {
			return err
		}
	}

	cursorPaths, err := paths.Resolve()
	if err != nil {
		return err
	}

	// -db 指向别的库时，「Cursor 必须退出」这条约束不适用：那条约束的存在原因是
	// Cursor 会把内存里的登录态刷回**它自己的**库，与其他库无关。
	dbPath := cursorPaths.StateDB
	customDB := false
	if *dbOverride != "" {
		dbPath = filepath.Clean(*dbOverride)
		customDB = dbPath != filepath.Clean(cursorPaths.StateDB)
	}

	fmt.Printf("目标账号   : %s（%s）\n", orDash(acc.Email), acc.UserID)
	fmt.Printf("套餐       : %s\n", orDash(acc.MembershipType))
	fmt.Printf("token 状态 : %s\n", expiryText(acc, time.Now()))
	fmt.Printf("目标库     : %s\n", dbPath)
	fmt.Printf("将写入 %d 个 key : %s\n", len(vscdb.SwitchWriteKeys), strings.Join(vscdb.SwitchWriteKeys, ", "))
	fmt.Printf("将删除 %d 个 key : %s\n", len(vscdb.SwitchDeleteKeys), strings.Join(vscdb.SwitchDeleteKeys, ", "))
	if missing := switcher.MissingKeys(acc); len(missing) > 0 {
		fmt.Printf("[注意] 以下 key 没有值，切过去后会保留旧账号的值：%s\n", strings.Join(missing, ", "))
	}

	// 先把 Cursor.exe 路径解析出来，再去关闭它。
	// 顺序很重要：procutil 优先用「进程正在运行该路径」当存在性证据，
	// 一旦关掉 Cursor 就只剩 os.Stat 可用，而本机 Cursor 装在 D 盘（坏盘）。
	var exePath string
	if *startCursor {
		loc, trace := procutil.Locate(procutil.DefaultProviders(ctx))
		exePath = loc.Path
		if exePath == "" {
			return errors.New("指定了 -start 但没能定位主程序，请去掉该参数后手工启动")
		}
		fmt.Printf("可执行文件 : %s\n", exePath)
		fmt.Printf("命中层级   : %s\n", loc.Tier)
		if loc.Detail != "" {
			fmt.Printf("命中明细   : %s\n", loc.Detail)
		}
		fmt.Println("查找过程   :")
		for _, line := range trace {
			fmt.Printf("  %s\n", line)
		}
	}

	// dry-run 必须在任何写入之前返回——包括写账号库。
	if *dryRun {
		fmt.Println("\n-dry-run：以上操作均未执行（账号库也没有写）。")
		return nil
	}

	// token 直切：先把账号存进账号库，再写 state.vscdb。
	// 顺序是刻意的——先把 token 落地，万一后面写库失败，凭据也已经存住了，
	// 用户可以直接 `acct switch <邮箱>` 重试，不必再去翻 token 的出处。
	if hasToken {
		if err := saveSwitchTarget(store, acc); err != nil {
			return err
		}
	}

	opt := switcher.Options{
		DBPath:       dbPath,
		BackupDir:    backupDir(store.Path()),
		AllowExpired: *allowExpired,
		Now:          time.Now(),
		EnsureCursorStopped: func() error {
			if customDB {
				fmt.Println("[提示] -db 指向的不是本机登录态的 state.vscdb，已跳过「编辑器必须退出」检查。")
				return nil
			}
			return ensureCursorStopped(ctx, *killCursor)
		},
	}

	rep, err := switcher.Switch(ctx, acc, opt)
	if err != nil {
		return err
	}

	fmt.Printf("\n已备份旧值 : %s\n", rep.BackupPath)
	fmt.Printf("已写入     : %s\n", strings.Join(rep.WrittenKeys, ", "))
	fmt.Printf("已删除     : %s\n", strings.Join(rep.DeletedKeys, ", "))
	fmt.Printf("typeof 校验: %d 个 key 均为 text（不是 blob）\n", len(rep.VerifiedText))
	fmt.Printf("journal    : %s，checkpoint busy=%d frames=%d 已回写=%d\n",
		rep.JournalMode, rep.Checkpoint.Busy, rep.Checkpoint.LogFrames, rep.Checkpoint.Checkpointed)

	if *startCursor {
		pid, err := procutil.StartCursor(exePath)
		if err != nil {
			return fmt.Errorf("切号已完成，但重新启动失败: %w", err)
		}
		fmt.Printf("已重新启动，PID %d\n", pid)
	} else {
		fmt.Println("\n切号完成，现在可以重新启动了。")
	}
	fmt.Printf("如需回退：acct rollback %s\n", rep.BackupPath)
	return nil
}

func runRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	killCursor := fs.Bool("kill", false, "编辑器还在跑时先优雅关闭")
	target, rest := splitPositional(args, nil)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if target == "" {
		return errors.New("请指定备份文件：acct rollback <备份文件>")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	b, err := switcher.LoadBackup(target)
	if err != nil {
		return err
	}
	fmt.Printf("备份创建于 : %s\n", b.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("目标库     : %s\n", b.DBPath)
	fmt.Printf("当时切往   : %s\n", orDash(b.SwitchedTo))

	// 与 switch 同一条规则：只有回滚的是本机 Cursor 自己的库时，
	// 「Cursor 必须退出」才有意义——它只会把内存里的登录态刷回那一个库。
	custom := true
	if cursorPaths, err := paths.Resolve(); err == nil {
		custom = filepath.Clean(b.DBPath) != filepath.Clean(cursorPaths.StateDB)
	}

	rep, err := switcher.Rollback(ctx, b, b.DBPath, func() error {
		if custom {
			fmt.Println("[提示] 备份指向的不是本机登录态的 state.vscdb，已跳过「编辑器必须退出」检查。")
			return nil
		}
		return ensureCursorStopped(ctx, *killCursor)
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n已恢复 : %s\n", strings.Join(rep.Restored, ", "))
	if len(rep.Removed) > 0 {
		fmt.Printf("已删除 : %s（切号前本就不存在）\n", strings.Join(rep.Removed, ", "))
	}
	fmt.Println("回滚完成。")
	return nil
}

// ensureCursorStopped 确认 Cursor 已退出；kill 为 true 时替用户关闭。
//
// 默认拒绝而不是直接关：把用户正在用的编辑器悄悄杀掉是很粗暴的行为，
// 未保存的编辑可能就没了。要关也先走优雅关闭（WM_CLOSE），超时才强杀。
func ensureCursorStopped(ctx context.Context, kill bool) error {
	records, err := procutil.ListProcesses(ctx)
	if err != nil {
		return fmt.Errorf("无法确认编辑器是否在运行: %w", err)
	}
	inv := procutil.Classify(records)
	if len(inv.Processes) == 0 {
		return nil // 没在跑，可以安全写入
	}

	mainProc, err := inv.SingleMain()
	if err != nil {
		return fmt.Errorf("无法确定编辑器主进程（%w）；请手工完全退出编辑器后重试", err)
	}
	if !kill {
		return &switcher.ErrCursorRunning{PID: mainProc.PID}
	}

	fmt.Printf("正在关闭编辑器（PID %d）：先发 WM_CLOSE，超时才强杀……\n", mainProc.PID)
	alive := func(ctx context.Context, pid int) (bool, error) {
		recs, err := procutil.ListProcesses(ctx)
		if err != nil {
			return false, err
		}
		for _, r := range recs {
			if r.PID == pid {
				return true, nil
			}
		}
		return false, nil
	}

	rep, err := procutil.StopCursor(ctx, mainProc.PID, procutil.DefaultStopOptions(),
		procutil.ExecRunner{}, alive)
	if err != nil {
		return fmt.Errorf("关闭编辑器失败: %w", err)
	}
	if rep.Escalated {
		fmt.Println("优雅关闭超时，已强制终止。未保存的编辑可能已丢失。")
	} else {
		fmt.Println("编辑器已正常退出。")
	}
	return nil
}

// switchValueFlags 是 switch 子命令里「后面还跟一个值」的 flag。
// 少列一个的后果是它的值会被当成位置参数（比如 -db 的路径被当成目标账号），
// 所以加新 flag 时必须同步维护这张表。
var switchValueFlags = map[string]bool{
	"-store": true, "--store": true,
	"-db": true, "--db": true,
	"-token": true, "--token": true,
	"-email": true, "--email": true,
	"-membership": true, "--membership": true,
	"-signup-type": true, "--signup-type": true,
}

// saveSwitchTarget 把 token 直切的账号写进账号库。
//
// 这里用**覆盖**语义，而不是像 import 那样默认拒绝：用户显式递了一个新 token 过来，
// 意图就是「用这个 token 登录」。此时因为同一用户 ID 已存在就报错要求加 -force，
// 只是白挡一道——旧记录里存的必然是同一个账号更早的 token，留着没有价值。
// 但覆盖了要打印出来，账号库不能被静默改写。
func saveSwitchTarget(store *acctstore.Store, acc acctstore.Account) error {
	if !acctstore.UserIDLooksStandard(acc.UserID) {
		fmt.Printf("[注意] 用户 ID %q 不是预期的 user_ 前缀，请确认 token 是否正确。\n", acc.UserID)
	}

	res, err := store.Upsert(acc, acctstore.ConflictOverwrite)
	if err != nil {
		return err
	}
	switch {
	case res.Created:
		fmt.Printf("已存入账号库: %s（新增，现有 %d 个账号）\n", store.Path(), res.Total)
	case res.Updated:
		fmt.Printf("已存入账号库: %s（覆盖了同一用户 ID 的旧记录，原记录导出于 %s）\n",
			store.Path(), res.Previous.ExportedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return nil
}

// splitPositional 取出第一个位置参数，其余原样交给 flag 解析。
//
// 为什么需要它：Go 的 flag 包遇到第一个位置参数就**停止解析**，所以
// `acct switch <id> -dry-run` 里的 -dry-run 会被当成普通参数丢掉，静默不生效。
// 而把参数写在目标后面是很自然的敲法，不能让用户以为 -dry-run 生效了却真的写了库。
//
// valueFlags 列出那些「后面还跟一个值」的 flag，避免把它们的值误当成位置参数
// （例如 `-store C:\path` 里的路径）。
func splitPositional(args []string, valueFlags map[string]bool) (string, []string) {
	var target string
	rest := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			if valueFlags[arg] && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if target == "" {
			target = arg
			continue
		}
		rest = append(rest, arg)
	}
	return target, rest
}

func backupDir(storePath string) string {
	return filepath.Join(filepath.Dir(storePath), "backups")
}

// resolveAccount 按用户 ID 或邮箱查账号，匹配不唯一时报错而不是瞎猜。
func resolveAccount(f acctstore.File, target string) (acctstore.Account, error) {
	if acc, _, ok := f.Find(target); ok {
		return acc, nil
	}

	var matches []acctstore.Account
	for _, a := range f.Accounts {
		if strings.EqualFold(a.Email, target) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		var known []string
		for _, a := range f.Accounts {
			known = append(known, fmt.Sprintf("%s (%s)", orDash(a.Email), a.UserID))
		}
		if len(known) == 0 {
			return acctstore.Account{}, fmt.Errorf("账号库里没有 %q，而且账号库是空的；"+
				"先用 acct save / acct import 加账号", target)
		}
		return acctstore.Account{}, fmt.Errorf("账号库里没有 %q。已有账号：\n       %s",
			target, strings.Join(known, "\n       "))
	default:
		var ids []string
		for _, a := range matches {
			ids = append(ids, a.UserID)
		}
		return acctstore.Account{}, fmt.Errorf("邮箱 %q 对应多个账号（%s），请改用用户 ID 指定",
			target, strings.Join(ids, ", "))
	}
}

// ---------------------------------------------------------------------------

func printAccount(a acctstore.Account, now time.Time) {
	fmt.Println("\n账号信息：")
	fmt.Printf("  邮箱       : %s\n", orDash(a.Email))
	fmt.Printf("  用户 ID    : %s\n", a.UserID)
	fmt.Printf("  原始 sub   : %s\n", orDash(a.Sub))
	fmt.Printf("  套餐       : %s\n", orDash(a.MembershipType))
	fmt.Printf("  注册方式   : %s\n", orDash(a.SignUpType))
	fmt.Printf("  token 状态 : %s\n", expiryText(a, now))
	fmt.Printf("  原始 key   : %d 个（%s）\n", len(a.Items), strings.Join(sortedKeys(a.Items), ", "))
}

func expiryText(a acctstore.Account, now time.Time) string {
	d, ok := a.ExpiresIn(now)
	if !ok {
		return "未知（token 里没有 exp）"
	}
	at := a.TokenExpiresAt.Local().Format("2006-01-02 15:04:05")
	if a.IsExpired(now) {
		return fmt.Sprintf("已过期（%s，过去了 %s）", at, roundDur(-d))
	}
	return fmt.Sprintf("有效（%s，还有 %s）", at, roundDur(d))
}

func roundDur(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%.1f 天", d.Hours()/24)
	}
	return d.Round(time.Minute).String()
}

// sortedKeys 按 vscdb 定义的顺序展示，缺失的自然不出现。
func sortedKeys(m map[string]string) []string {
	ordered := make([]string, 0, len(m))
	for _, k := range vscdb.AuthKeys {
		if _, ok := m[k]; ok {
			ordered = append(ordered, k)
		}
	}
	return ordered
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}
