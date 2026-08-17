// Command probe 是一个只读探测工具：读取本机 Cursor 登录态并调用 Cursor 的
// 用量接口，用于验证「Go 重写」这条链路可行。
//
// 安全边界（贯穿全程，请勿在后续修改中破坏）：
//   - 只读打开 state.vscdb，不写、不删、不复制整库；
//   - 不启动也不终止 Cursor 进程。
//
// token / 邮箱 / 用户 ID 一律明文输出（本地个人工具，遮蔽只会妨碍排查）。
// 因此 probe-output/ 会含明文凭据，已在 .gitignore 中排除，切勿提交。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Nihility981/p-roll-cursor/internal/cursorapi"
	"github.com/Nihility981/p-roll-cursor/internal/jwtutil"
	"github.com/Nihility981/p-roll-cursor/internal/mask"
	"github.com/Nihility981/p-roll-cursor/internal/paths"
	"github.com/Nihility981/p-roll-cursor/internal/procutil"
	"github.com/Nihility981/p-roll-cursor/internal/usage"
	"github.com/Nihility981/p-roll-cursor/internal/vscdb"
)

func main() {
	flag.Usage = func() {
		fmt.Print(`probe —— 只读探测本机登录态与用量

参数：
`)
		flag.PrintDefaults()
	}
	outDir := flag.String("out", "probe-output", "原始响应留档目录")
	skipNet := flag.Bool("offline", false, "跳过所有网络请求，只做本地读取")
	flag.Parse()

	if err := run(*outDir, *skipNet); err != nil {
		fmt.Fprintf(os.Stderr, "\n[致命错误] %v\n", err)
		os.Exit(1)
	}
}

func run(outDir string, offline bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	err := probe(ctx, outDir, offline)
	// 第 7 段是纯本地发现，与网络链路无关：前面无论早退还是出错都要跑，
	// 否则 -offline 和接口失败的场景就看不到进程信息了。
	section7Process(ctx)
	return err
}

func probe(ctx context.Context, outDir string, offline bool) error {
	cursorPaths, err := section1Env()
	if err != nil {
		return err
	}

	authState, err := section2Auth(ctx, cursorPaths)
	if err != nil {
		return err
	}

	candidates, err := section3JWT(authState)
	if err != nil {
		return err
	}

	if offline {
		fmt.Println("\n已指定 -offline，跳过全部网络请求。")
		return nil
	}

	client := cursorapi.New()
	meta := section4UserMeta(ctx, client, authState.AccessToken)
	section4Stripe(ctx, client, authState.AccessToken)

	if meta != nil && strings.TrimSpace(meta.WorkosID) != "" {
		candidates = append(candidates, candidate{
			Name:   "GetUserMeta.workosId",
			Value:  strings.TrimSpace(meta.WorkosID),
			Source: "GetUserMeta 响应",
		})
	}

	raw, rawBody := section4Usage(ctx, client, authState.AccessToken, candidates)
	if raw == nil {
		fmt.Println("\n没有任何认证组合成功拿到 usage-summary，后续小节跳过。")
		return nil
	}

	section5Usage(raw, authState)
	return section6Dump(outDir, raw, rawBody)
}

// ---------------------------------------------------------------------------
// 1. 环境信息
// ---------------------------------------------------------------------------

func section1Env() (*paths.CursorPaths, error) {
	header("1. 环境信息")

	p, err := paths.Resolve()
	if err != nil {
		return nil, err
	}

	fmt.Printf("  本机数据目录    : %s\n", p.AppDir)
	fmt.Printf("  globalStorage   : %s\n", p.GlobalStorage)

	db := paths.Stat(p.StateDB)
	if !db.Exists {
		return nil, fmt.Errorf("未找到 state.vscdb：%s（编辑器是否安装并登录过？）", p.StateDB)
	}
	fmt.Printf("  state.vscdb     : %s (%s)\n", p.StateDB, paths.HumanSize(db.Size))

	// 用有序 slice 而不是 map，保证多次运行的输出可以直接 diff。
	for _, f := range []struct{ label, path string }{
		{"-wal 文件       ", p.StateDBWAL},
		{"-shm 文件       ", p.StateDBSHM},
		{"options.json    ", p.OptionsFile},
	} {
		label, info := f.label, paths.Stat(f.path)
		if info.Exists {
			fmt.Printf("  %s: 存在 (%s)\n", label, paths.HumanSize(info.Size))
		} else {
			fmt.Printf("  %s: 不存在\n", label)
		}
	}

	if content, err := os.ReadFile(p.OptionsFile); err == nil {
		fmt.Printf("  options.json 内容: %s\n", strings.TrimSpace(string(content)))
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 2. 本机登录态
// ---------------------------------------------------------------------------

func section2Auth(ctx context.Context, p *paths.CursorPaths) (*vscdb.AuthState, error) {
	header("2. 本机登录态（state.vscdb 只读）")

	started := time.Now()
	db, err := vscdb.Open(p.StateDB)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	fmt.Printf("  只读打开耗时    : %s\n", time.Since(started).Round(time.Millisecond))

	if mode, err := db.JournalMode(ctx); err == nil {
		fmt.Printf("  journal_mode    : %s\n", mode)
	} else {
		fmt.Printf("  journal_mode    : 读取失败(%v)\n", err)
	}

	// 查询仍用真实表名；打到终端的标签不用品牌词。
	tableLabel := map[string]string{"cursorDiskKV": "磁盘KV"}
	for _, stat := range db.TableStats(ctx, []string{"ItemTable", "cursorDiskKV", "composerHeaders"}) {
		label := stat.Name
		if alt, ok := tableLabel[stat.Name]; ok {
			label = alt
		}
		if stat.Err != nil {
			fmt.Printf("  表 %-16s: 统计失败(%v)\n", label, stat.Err)
			continue
		}
		fmt.Printf("  表 %-16s: %d 行\n", label, stat.Rows)
	}

	state, err := vscdb.LoadAuthState(ctx, db)
	if err != nil {
		return nil, err
	}

	fmt.Println("\n  认证相关 key：")
	for _, key := range vscdb.AuthKeys {
		item := state.Items[key]
		if !item.Exists {
			fmt.Printf("    %-38s ✗ 不存在\n", key)
			continue
		}
		fmt.Printf("    %-38s ✓ typeof=%-4s len=%-4d 值=%s\n",
			key, item.TypeOf, len(item.Value), describeValue(key, item.Value))
	}

	if state.AccessToken == "" {
		return nil, fmt.Errorf("state.vscdb 中没有 %s，本机未登录", vscdb.KeyAccessToken)
	}
	if state.RefreshToken != "" && state.RefreshToken == state.AccessToken {
		fmt.Println("\n  注意：refreshToken 与 accessToken 完全相同（既有行为，非数据错误）")
	}
	return state, nil
}

// describeValue 按 key 语义选择脱敏方式。
func describeValue(key, value string) string {
	switch key {
	case vscdb.KeyAccessToken, vscdb.KeyRefreshToken, vscdb.KeyCachedScopedProfile,
		vscdb.KeyLegacyAccessToken:
		return mask.Token(value)
	case vscdb.KeyCachedEmail, vscdb.KeyLegacyEmail:
		return mask.Email(value)
	case vscdb.KeyStripeMembershipAuthID, vscdb.KeyLegacyAuthID:
		return fmt.Sprintf("%s 形态=%s", mask.ID(value), mask.Shape(value))
	default:
		return value
	}
}

// ---------------------------------------------------------------------------
// 3. JWT 解析
// ---------------------------------------------------------------------------

// candidate 是一个待验证的 usage-summary cookie userId 候选值。
type candidate struct {
	Name   string
	Value  string
	Source string
	// RustAccepted 表示 Rust 参照实现是否会接受这个值（要求 user_ 前缀）。
	RustAccepted bool
}

func section3JWT(state *vscdb.AuthState) ([]candidate, error) {
	header("3. JWT 解析")

	payload, err := jwtutil.Decode(state.AccessToken)
	if err != nil {
		return nil, err
	}

	fmt.Printf("  sub             : %s\n", mask.ID(payload.Sub))
	fmt.Printf("  sub 形态        : %s\n", mask.Shape(payload.Sub))
	fmt.Printf("  sub 长度        : %d\n", len(payload.Sub))
	fmt.Printf("  iss             : %s\n", payload.Iss)
	if payload.Scope != "" {
		fmt.Printf("  scope           : %s\n", payload.Scope)
	}

	if payload.Exp > 0 {
		expiry := payload.TimeToExpiry()
		state := "有效"
		if expiry <= 0 {
			state = "已过期"
		}
		fmt.Printf("  exp             : %d (%s, %s, 剩余 %s)\n",
			payload.Exp, payload.ExpiresAt().Local().Format(time.RFC3339), state,
			expiry.Round(time.Minute))
	} else {
		fmt.Println("  exp             : 缺失")
	}

	if payload.Raw != nil {
		keys := make([]string, 0, len(payload.Raw))
		for k := range payload.Raw {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("  payload 顶层 key: %s\n", strings.Join(keys, ", "))
	}

	lastSegment, rustOK := jwtutil.WorkosUserIDFromSub(payload.Sub)
	fmt.Printf("\n  按 '|' 切最后一段: %s 形态=%s\n", mask.ID(lastSegment), mask.Shape(lastSegment))
	if rustOK {
		fmt.Println("  是否以 user_ 开头: 是 → Rust 逻辑可用")
	} else {
		fmt.Println("  是否以 user_ 开头: 否 → Rust 逻辑会直接判定「无法解析 WorkOS 用户 ID」并放弃请求")
	}

	// 这里一律登记全部候选，即使某几个值相同也不在此处剔除：
	// 去重与「已跳过」的说明统一由 section4Usage 负责，避免候选凭空消失。
	candidates := []candidate{
		{Name: "sub 切 '|' 最后一段(Rust 逻辑)", Value: lastSegment, Source: "JWT.sub", RustAccepted: rustOK},
		{Name: "完整 sub(不切)", Value: payload.Sub, Source: "JWT.sub"},
	}
	if state.StripeMembershipAuthID != "" {
		candidates = append(candidates, candidate{
			Name:   "stripeMembershipAuthId",
			Value:  state.StripeMembershipAuthID,
			Source: "state.vscdb",
		})
	}
	return candidates, nil
}

// ---------------------------------------------------------------------------
// 4. API 调用
// ---------------------------------------------------------------------------

func section4UserMeta(ctx context.Context, client *cursorapi.Client, token string) *cursorapi.UserMeta {
	header("4a. GetUserMeta")

	res, meta, err := client.GetUserMeta(ctx, token)
	if err != nil {
		fmt.Printf("  失败: %v\n", err)
		if res != nil {
			fmt.Printf("  状态码: %d 响应片段: %s\n", res.StatusCode, res.BodySnippet(200))
		}
		return nil
	}
	fmt.Printf("  POST %s\n", cursorapi.GetUserMetaURL)
	fmt.Printf("  状态码          : %d (%s)\n", res.StatusCode, res.Duration.Round(time.Millisecond))
	if meta == nil {
		fmt.Printf("  响应片段        : %s\n", res.BodySnippet(300))
		return nil
	}
	fmt.Printf("  email           : %s\n", mask.Email(meta.Email))
	fmt.Printf("  signUpType      : %s\n", orDash(meta.SignUpType))
	fmt.Printf("  workosId        : %s 形态=%s\n", mask.ID(meta.WorkosID), mask.Shape(meta.WorkosID))
	return meta
}

func section4Stripe(ctx context.Context, client *cursorapi.Client, token string) {
	header("4b. Stripe profile")

	outcome, err := client.FetchStripeProfile(ctx, token)
	if err != nil {
		fmt.Printf("  失败: %v\n", err)
		return
	}
	if outcome.Full != nil {
		fmt.Printf("  GET full_stripe_profile → %d (%s)\n",
			outcome.Full.StatusCode, outcome.Full.Duration.Round(time.Millisecond))
	}
	if outcome.Fallback != nil {
		fmt.Printf("  GET stripe_profile(回退) → %d (%s)\n",
			outcome.Fallback.StatusCode, outcome.Fallback.Duration.Round(time.Millisecond))
	}
	if outcome.Profile == nil {
		fmt.Println("  两个端点都未返回可解析的 profile")
		return
	}
	p := outcome.Profile
	fmt.Printf("  采用端点        : %s\n", outcome.UsedURL)
	fmt.Printf("  membershipType  : %s\n", orDash(p.MembershipType))
	fmt.Printf("  individualMembershipType: %s\n", orDash(p.IndividualMembershipType))
	fmt.Printf("  subscriptionStatus      : %s\n", orDash(p.SubscriptionStatus))
	fmt.Printf("  teamMembershipType      : %s\n", orDash(p.TeamMembershipType))
	fmt.Printf("  isTeamMember / isEnterprise: %v / %v\n", p.IsTeamMember, p.IsEnterprise)
	fmt.Printf("  归一化徽章      : %s\n", usage.BadgeFromMembershipType(p.MembershipType))
}

// section4Usage 逐个尝试候选认证方式，返回第一个成功的响应。
func section4Usage(ctx context.Context, client *cursorapi.Client, token string,
	candidates []candidate) (map[string]any, []byte) {

	header("4c. usage-summary（多候选对照）")

	type attempt struct {
		label string
		// cookieValue 为空表示这一路不带 Cookie，不参与去重。
		cookieValue string
		opts        cursorapi.UsageSummaryOptions
	}

	attempts := make([]attempt, 0, len(candidates)+1)
	for _, c := range candidates {
		suffix := ""
		if c.RustAccepted {
			suffix = " [Rust 会采用]"
		}
		attempts = append(attempts, attempt{
			label:       fmt.Sprintf("Cookie userId = %s (来源 %s)%s", c.Name, c.Source, suffix),
			cookieValue: c.Value,
			opts:        cursorapi.UsageSummaryOptions{CookieUserID: c.Value, AccessToken: token},
		})
	}
	attempts = append(attempts, attempt{
		label: "不带 Cookie，仅 Authorization: Bearer",
		opts:  cursorapi.UsageSummaryOptions{AccessToken: token, WithBearer: true},
	})

	var winner *cursorapi.Result
	var winnerLabel string
	// firstUse 记录每个 cookie 值首次出现在第几号候选，用于去重时明确指向。
	firstUse := make(map[string]int, len(attempts))

	// 刻意不在拿到第一个成功结果后 break：本节的目的就是横向对照每种认证组合
	// 的真实表现，必须把所有候选都跑一遍。生产路径应当在首个成功后短路。
	for i, a := range attempts {
		num := i + 1
		fmt.Printf("\n  [%d] %s\n", num, a.label)

		if a.cookieValue != "" {
			if prev, dup := firstUse[a.cookieValue]; dup {
				fmt.Printf("      已跳过：值与候选 [%d] 完全相同（%s），不重复请求\n",
					prev, a.cookieValue)
				continue
			}
			firstUse[a.cookieValue] = num
		}

		res, err := client.FetchUsageSummary(ctx, a.opts)
		if err != nil {
			fmt.Printf("      请求失败: %v\n", err)
			continue
		}
		fmt.Printf("      状态码 %d，耗时 %s，Content-Type %s，响应 %d 字节\n",
			res.StatusCode, res.Duration.Round(time.Millisecond),
			orDash(res.ContentType), len(res.Body))
		if res.StatusCode != 200 || !res.IsJSON() {
			fmt.Printf("      响应片段: %s\n", res.BodySnippet(160))
			continue
		}
		fmt.Println("      ✓ 成功拿到 JSON")
		if winner == nil {
			winner = res
			winnerLabel = a.label
		}
	}

	if winner == nil {
		return nil, nil
	}

	fmt.Printf("\n  结论：采用「%s」\n", winnerLabel)
	var raw map[string]any
	if err := json.Unmarshal(winner.Body, &raw); err != nil {
		fmt.Printf("  解析 usage-summary JSON 失败: %v\n", err)
		return nil, winner.Body
	}
	return raw, winner.Body
}

// ---------------------------------------------------------------------------
// 5. 结构化用量
// ---------------------------------------------------------------------------

func section5Usage(raw map[string]any, state *vscdb.AuthState) {
	header("5. 结构化用量")

	s := usage.Parse(raw)
	if s.MembershipType == "" {
		// 响应里没带套餐时回退到本机缓存值。
		s.MembershipType = state.MembershipType
		s.Badge = usage.BadgeFromMembershipType(state.MembershipType)
	}

	fmt.Printf("  套餐 membership_type : %s → 徽章 %s\n", orDash(s.MembershipType), s.Badge)
	fmt.Printf("  totalPercentUsed     : %s\n", usage.FormatPercent(s.TotalPercentUsed))
	fmt.Printf("  autoPercentUsed      : %s\n", usage.FormatPercent(s.AutoPercentUsed))
	fmt.Printf("  apiPercentUsed       : %s\n", usage.FormatPercent(s.APIPercentUsed))
	fmt.Printf("  plan used / limit    : %s / %s\n",
		usage.FormatCents(s.PlanUsedCents), usage.FormatCents(s.PlanLimitCents))
	fmt.Printf("  breakdown 用量构成   : included %s + bonus %s = total %s\n",
		usage.FormatCents(s.PlanBreakdownIncludedCents),
		usage.FormatCents(s.PlanBreakdownBonusCents),
		usage.FormatCents(s.PlanBreakdownTotalCents))
	fmt.Printf("  自算百分比(used/limit): %s\n", usage.FormatPercent(s.DerivedPercent))
	fmt.Printf("  最终展示百分比       : %s\n", usage.FormatPercent(s.EffectivePercent))

	printAllowance(s.Allowance)

	od := s.OnDemand()
	// 两侧数值都打印出来：探测阶段信息越全越好，而且「字段缺失(—)」和
	// 「值为 0」必须能区分开，否则排查时会被误导。
	fmt.Printf("\n  On-Demand 已用       : 团队 %s / 个人 %s（实际采用 %s）\n",
		usage.FormatCents(od.TeamUsedCents), usage.FormatCents(od.IndividualUsedCents), od.UsedSource)
	fmt.Printf("  On-Demand 上限       : 团队 %s / 个人 %s（实际采用 %s）\n",
		usage.FormatCents(od.TeamLimitCents), usage.FormatCents(od.IndividualLimitCents), od.LimitSource)
	fmt.Printf("  onDemandEnabled      : %s\n", boolPtr(s.OnDemandEnabled))
	fmt.Printf("  limitType            : %s (isTeamLimit=%v)\n", orDash(s.OnDemandLimitType), od.IsTeamLimit)
	fmt.Printf("  isUnlimited          : %v (顶层字段 %v)\n", od.IsUnlimited, s.IsUnlimited)
	fmt.Printf("  状态                 : %s\n", onDemandState(od))

	if s.AllowanceResetAt != nil {
		fmt.Printf("\n  额度重置时间         : %s (还有 %s)\n",
			time.Unix(*s.AllowanceResetAt, 0).Local().Format(time.RFC3339),
			s.AllowanceResetIn.Round(time.Minute))
	} else {
		fmt.Println("\n  额度重置时间         : 响应中未找到 billingCycleEnd")
	}

	fmt.Printf("\n  命中的字段路径       : %s\n", joinOrDash(s.FoundPaths))
	fmt.Printf("  未命中的假设路径     : %s\n", joinOrDash(s.MissingPaths))
}

// printAllowance 展示从响应反解出来的额度口径。
//
// 响应里没有任何字段直接写出分母，这里全部由 breakdown.total 与三个百分比
// 解方程得到，**不是硬编码**——换一个额度不同的账号会自动显示那个账号的
// 真实分母。校验不通过即意味着 Cursor 可能改了计费模型。
func printAllowance(m *usage.AllowanceModel) {
	if m == nil {
		fmt.Println("\n  用量口径推导         : 条件不足（缺 breakdown.total 或 totalPercentUsed），无法反解分母")
		return
	}

	if m.SplitSolved {
		fmt.Printf("\n  用量口径推导         : API  %s / %s = %s\n",
			cents(m.APIUsedCents), cents(m.APICents), percent(m.APIUsedCents, m.APICents))
		fmt.Printf("                         auto %s / %s = %s\n",
			cents(m.AutoUsedCents), cents(m.AutoCents), percent(m.AutoUsedCents, m.AutoCents))
		fmt.Printf("                         合计 %s / %s = %.3f%%\n",
			cents(m.TotalUsedCents), cents(m.TotalCents), m.DerivedPercent)
	} else {
		fmt.Printf("\n  用量口径推导         : 合计 %s / %s = %.3f%%（API/auto 拆分不可解）\n",
			cents(m.TotalUsedCents), cents(m.TotalCents), m.DerivedPercent)
	}

	if m.Consistent {
		fmt.Println("  口径自洽校验         : ✓ 闭合（各项均为整数分，重算占比与 totalPercentUsed 一致）")
		return
	}
	fmt.Println("  口径自洽校验         : ✗ 不闭合，计费模型可能已变更：")
	for _, note := range m.Notes {
		fmt.Printf("                         - %s\n", note)
	}
}

// cents 把整数分格式化成「7192 分($71.92)」。
func cents(v float64) string {
	return fmt.Sprintf("%.0f 分($%.2f)", v, v/100)
}

func percent(used, total float64) string {
	if total == 0 {
		return "—"
	}
	return fmt.Sprintf("%.3f%%", used/total*100)
}

func onDemandState(od usage.OnDemandView) string {
	switch {
	case od.IsDisabled:
		return "未开启"
	case od.HasFixedLimit:
		return "已开启，有固定上限"
	case od.IsTeamManagedLimit:
		return "已开启，无个人固定上限（上限由团队侧管理）"
	case od.IsUnlimited:
		return "已开启且无固定上限"
	default:
		return "状态未知"
	}
}

// ---------------------------------------------------------------------------
// 6. 原始响应留档
// ---------------------------------------------------------------------------

func section6Dump(outDir string, raw map[string]any, body []byte) error {
	header("6. 原始响应留档")

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("  顶层 key (%d 个)：%s\n", len(keys), strings.Join(keys, ", "))
	fmt.Printf("  原始响应大小    : %d 字节\n\n", len(body))

	fmt.Println("  结构概览：")
	for _, line := range mask.Outline(raw, 3) {
		fmt.Printf("    %s\n", line)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("创建留档目录 %s 失败: %w", outDir, err)
	}
	target := filepath.Join(outDir, "usage-raw.json")

	encoded, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化响应失败: %w", err)
	}
	if err := os.WriteFile(target, encoded, 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", target, err)
	}

	abs, _ := filepath.Abs(target)
	fmt.Printf("\n  已写入原始响应: %s (%d 字节)\n", abs, len(encoded))
	return nil
}

// ---------------------------------------------------------------------------
// 7. 进程与路径管理（只读预演）
// ---------------------------------------------------------------------------

// section7Process 报告「如果要切号，我会怎么操作」，但一个操作都不执行。
//
// 之所以只做预演：开发这套工具用的就是 Cursor 本身，真去关闭它，当前会话会
// 立刻断掉。本段只做发现（读注册表、查 WMI），不改变 probe 的只读性质。
func section7Process(ctx context.Context) {
	header("7. 进程与路径管理（只读预演，不执行任何操作）")

	providers := procutil.DefaultProviders(ctx)

	records, err := providers.Processes()
	if err != nil {
		fmt.Printf("  进程枚举失败       : %v\n", err)
	}
	inv := procutil.Classify(records)

	loc, trace := procutil.Locate(providers)
	fmt.Printf("  主程序路径         : %s\n", orDash(loc.Path))
	if loc.Path != "" {
		fmt.Printf("    命中层级         : %s\n", loc.Tier)
		fmt.Printf("    命中明细         : %s\n", loc.Detail)
		fmt.Printf("    存在性证据       : %s\n", loc.Evidence)
		fmt.Printf("    是否访问过磁盘   : %v\n", loc.TouchedDisk)
	}
	fmt.Println("    查找过程         :")
	for _, line := range trace {
		fmt.Printf("      %s\n", line)
	}

	fmt.Printf("\n  进程总数          : %d\n", len(inv.Processes))
	for _, c := range inv.Categories() {
		fmt.Printf("    %s %2d 个  PID %s\n", padDisplay(c.Label, 30), len(c.PIDs), joinInts(c.PIDs))
	}

	mainProc, err := inv.SingleMain()
	if err != nil {
		fmt.Printf("\n  主进程识别         : 失败 —— %v\n", err)
		fmt.Println("\n  未能确定唯一主进程，不生成操作预演。")
		return
	}

	fmt.Printf("\n  主进程 PID         : %d\n", mainProc.PID)
	fmt.Printf("    判定依据         : %s\n", mainProc.Reason)
	fmt.Printf("    父进程 PID       : %d\n", mainProc.ParentPID)
	fmt.Printf("    ExecutablePath   : %s\n", orDash(mainProc.ExePath))
	// 主进程命令行里没有 --user-data-dir，只能从子进程反查。
	fmt.Printf("    主进程自带的 user-data-dir : %s\n", orDash(mainProc.UserDataDir))
	fmt.Printf("    从子进程反查得到           : %s\n", joinOrDash(inv.UserDataDirs(mainProc.PID)))

	opt := procutil.DefaultStopOptions()
	fmt.Println("\n  拟执行但【未执行】的操作：")
	fmt.Printf("    1) %s\n", strings.Join(procutil.GracefulKillCommand(mainProc.PID), " "))
	fmt.Println("       └ 优雅关闭，不带 /F，发的是 WM_CLOSE，编辑器有机会保存未提交的编辑")
	fmt.Printf("    2) 每 %v 轮询一次，最多等 %v\n", opt.PollInterval, opt.GraceTimeout)
	fmt.Printf("    3) %s\n", strings.Join(procutil.ForceKillCommand(mainProc.PID), " "))
	fmt.Printf("       └ 仅在第 2 步超时后才升级为强杀；/T 连子进程整棵树，未保存的编辑会丢\n")
	fmt.Printf("    4) %s\n", strings.Join(procutil.StartCommand(loc.Path), " "))
	fmt.Println("       └ 切号完成后重新启动")
	fmt.Println("\n  以上命令一条都没有运行：本段只做发现与报告，probe 不调用关闭/启动逻辑。")
}

// padDisplay 按终端显示宽度右侧补空格。不能直接用 %-Ns：那按字节数算，
// 中日韩字符占两列却算三字节，列会歪。
func padDisplay(s string, width int) string {
	w := 0
	for _, r := range s {
		if r > 0x2E7F {
			w += 2
		} else {
			w++
		}
	}
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func joinInts(v []int) string {
	if len(v) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(v))
	for _, n := range v {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------

func header(title string) {
	fmt.Printf("\n%s\n=== %s ===\n", strings.Repeat("─", 72), title)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func boolPtr(b *bool) string {
	if b == nil {
		return "—"
	}
	return fmt.Sprintf("%v", *b)
}

func joinOrDash(items []string) string {
	if len(items) == 0 {
		return "—"
	}
	return strings.Join(items, ", ")
}
