// Package usage 解析 usage-summary 的响应。
//
// 解析规则从前端 TS（src/types/cursor.ts 的 getCursorUsage）迁移而来：
// Cursor 的这个接口在不同版本/不同账号类型下会返回 camelCase 或 snake_case，
// 因此每个字段都要按候选 key 逐个尝试。
package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// PlanBadge 是归一化后的套餐标识。
type PlanBadge string

const (
	BadgeFree       PlanBadge = "FREE"
	BadgePro        PlanBadge = "PRO"
	BadgeProPlus    PlanBadge = "PRO_PLUS"
	BadgeEnterprise PlanBadge = "ENTERPRISE"
	BadgeFreeTrial  PlanBadge = "FREE_TRIAL"
	BadgeUltra      PlanBadge = "ULTRA"
	BadgeUnknown    PlanBadge = "UNKNOWN"
)

// NormalizeMembershipType 归一化 membership_type：
// pro_student 视为 PRO，business/team 视为 ENTERPRISE。
func NormalizeMembershipType(raw string) string {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	switch normalized {
	case "":
		return ""
	case "pro_student":
		return "pro"
	case "business", "team":
		return "enterprise"
	default:
		return normalized
	}
}

// BadgeFromMembershipType 把 membership_type 映射成徽章。
func BadgeFromMembershipType(raw string) PlanBadge {
	switch NormalizeMembershipType(raw) {
	case "free":
		return BadgeFree
	case "pro":
		return BadgePro
	case "pro_plus":
		return BadgeProPlus
	case "enterprise":
		return BadgeEnterprise
	case "free_trial":
		return BadgeFreeTrial
	case "ultra":
		return BadgeUltra
	case "":
		return BadgeUnknown
	default:
		return PlanBadge(strings.ToUpper(NormalizeMembershipType(raw)))
	}
}

// Summary 是解析后的结构化用量。指针字段为 nil 表示响应里没有该项。
type Summary struct {
	TotalPercentUsed *float64
	AutoPercentUsed  *float64
	APIPercentUsed   *float64

	PlanUsedCents  *float64
	PlanLimitCents *float64

	// PlanBreakdown* 来自 plan.breakdown，描述「已消耗用量」的构成：
	// total = included + bonus。注意 total 是**用量**而不是额度上限，
	// 详见 derivePercent 的说明。
	PlanBreakdownIncludedCents *float64
	PlanBreakdownBonusCents    *float64
	PlanBreakdownTotalCents    *float64

	// DerivedPercent 是 used/limit 自算出来的百分比。只有在 limit 确实覆盖
	// 全部额度时才有值，否则为 nil（宁可留空也不给误导性数字）。
	DerivedPercent *float64
	// EffectivePercent 是最终对外展示的百分比（优先 total，其次自算，钳制到 0~100）。
	EffectivePercent *float64

	OnDemandUsedCents      *float64
	OnDemandLimitCents     *float64
	TeamOnDemandUsedCents  *float64
	TeamOnDemandLimitCents *float64
	OnDemandEnabled        *bool
	OnDemandLimitType      string

	IsUnlimited bool

	// AllowanceResetAt 来自 billingCycleEnd，Unix 秒。
	AllowanceResetAt *int64
	AllowanceResetIn time.Duration

	MembershipType string
	Badge          PlanBadge

	// Allowance 是从响应自身反解出来的额度口径（分母）。响应里没有任何字段
	// 直接给出这些数字，nil 表示条件不足、推不出来。
	Allowance *AllowanceModel

	// FoundPaths 记录实际命中的字段路径，用于确认真实响应结构。
	FoundPaths []string
	// MissingPaths 记录 TS 里假设存在但本次没命中的路径。
	MissingPaths []string
}

// Parse 从 usage-summary 的原始 JSON 解析出结构化用量。
func Parse(raw map[string]any) *Summary {
	s := &Summary{}

	plan, planPath := firstPath(raw,
		[]string{"individualUsage", "plan"},
		[]string{"individual_usage", "plan"},
		[]string{"planUsage"},
		[]string{"plan_usage"},
	)
	s.note(planPath != "", "plan="+orNone(planPath), "plan(individualUsage.plan/planUsage)")

	individualOnDemand, iodPath := firstPath(raw,
		[]string{"individualUsage", "onDemand"},
		[]string{"individual_usage", "on_demand"},
		[]string{"individual_usage", "onDemand"},
	)
	s.note(iodPath != "", "onDemand="+orNone(iodPath), "onDemand(individualUsage.onDemand)")

	teamOnDemand, todPath := firstPath(raw,
		[]string{"teamUsage", "onDemand"},
		[]string{"team_usage", "on_demand"},
		[]string{"team_usage", "onDemand"},
	)
	s.note(todPath != "", "teamOnDemand="+orNone(todPath), "teamOnDemand(teamUsage.onDemand)")

	spendLimitUsage, slPath := firstPath(raw,
		[]string{"spendLimitUsage"},
		[]string{"spend_limit_usage"},
	)
	s.note(slPath != "", "spendLimitUsage="+orNone(slPath), "spendLimitUsage")

	onDemand := individualOnDemand
	if onDemand == nil {
		onDemand = spendLimitUsage
	}

	s.TotalPercentUsed = pickNumber(plan, "totalPercentUsed", "total_percent_used")
	s.AutoPercentUsed = pickNumber(plan, "autoPercentUsed", "auto_percent_used")
	s.APIPercentUsed = pickNumber(plan, "apiPercentUsed", "api_percent_used")
	s.PlanUsedCents = pickNumber(plan, "used", "totalSpend", "total_spend")
	s.PlanLimitCents = pickNumber(plan, "limit")

	breakdown, bdPath := firstPath(asMap(plan),
		[]string{"breakdown"},
		[]string{"break_down"},
	)
	s.note(bdPath != "", "breakdown=plan."+orNone(bdPath), "breakdown(plan.breakdown)")
	s.PlanBreakdownIncludedCents = pickNumber(breakdown, "included", "included_cents")
	s.PlanBreakdownBonusCents = pickNumber(breakdown, "bonus", "bonus_cents")
	s.PlanBreakdownTotalCents = pickNumber(breakdown, "total", "total_cents")

	s.OnDemandUsedCents = pickNumber(onDemand,
		"used", "totalSpend", "total_spend", "individualUsed", "individual_used")
	s.OnDemandLimitCents = pickNumber(onDemand,
		"limit", "individualLimit", "individual_limit", "pooledLimit", "pooled_limit")

	s.TeamOnDemandUsedCents = firstNonNil(
		pickNumber(teamOnDemand, "used"),
		pickNumber(spendLimitUsage, "pooledUsed", "pooled_used", "overallUsed", "overall_used"),
	)
	s.TeamOnDemandLimitCents = firstNonNil(
		pickNumber(teamOnDemand, "limit"),
		pickNumber(spendLimitUsage, "pooledLimit", "pooled_limit", "overallLimit", "overall_limit"),
	)
	s.OnDemandEnabled = pickBool(individualOnDemand, "enabled")

	s.IsUnlimited = boolValue(raw, "isUnlimited") || boolValue(raw, "is_unlimited")

	limitType := pickString(raw, "limitType", "limit_type")
	if limitType == "" {
		limitType = pickString(spendLimitUsage, "limitType", "limit_type")
	}
	s.OnDemandLimitType = strings.ToLower(strings.TrimSpace(limitType))

	if billingEnd := pickString(raw, "billingCycleEnd", "billing_cycle_end"); billingEnd != "" {
		if t, err := time.Parse(time.RFC3339, billingEnd); err == nil {
			ts := t.Unix()
			s.AllowanceResetAt = &ts
			s.AllowanceResetIn = time.Until(t)
			s.FoundPaths = append(s.FoundPaths, "billingCycleEnd")
		}
	} else {
		s.MissingPaths = append(s.MissingPaths, "billingCycleEnd")
	}

	s.DerivedPercent = derivePercent(s.PlanUsedCents, s.PlanLimitCents, s.PlanBreakdownTotalCents)

	base := s.TotalPercentUsed
	if base == nil {
		base = s.DerivedPercent
	}
	if base != nil {
		// 与前端一致：>0 但 <1 的极小值直接抬到 1%，避免显示成 0。
		v := *base
		if v > 0 && v < 1 {
			v = 1
		}
		v = math.Min(100, math.Max(0, v))
		s.EffectivePercent = &v
	}

	s.MembershipType = pickString(raw, "membershipType", "membership_type")
	s.Badge = BadgeFromMembershipType(s.MembershipType)

	s.Allowance = deriveAllowance(s)

	return s
}

// AllowanceModel 是从 usage-summary 响应反解出来的额度口径。
//
// 背景：响应里给的全是百分比，**没有任何字段直接写出额度分母**。但把
// breakdown.total（已消耗总量）和三个百分比放在一起，分母是可解的：
//
//	设 API 分母为 Da、auto 分母为 Db，api/auto/total 三个百分比为 a、b、t，
//	已消耗总量为 T，则
//	    T / (t/100) = Da + Db        …… 总分母
//	    a*Da + b*Db = T              …… 两路用量相加等于总用量
//	两个方程两个未知数，可以直接解出 Da、Db。
//
// 本机实测（三次抓取全部吻合，解出的六个值都是精确整数分）：
//
//	Da = 20000 分（$200，对应响应文案 "included API usage"）
//	Db = 70000 分（$700）
//	Da + Db = 90000 分（$900，对应文案 "included total usage"）
//
// 注意：因为是解方程，「两路相加等于总量」是恒等式、不构成独立检验。真正的
// 独立证据是解出来的数值都落在整数分上——Cursor 一旦改计费模型，反解就会
// 出现带小数的脏数字，Consistent 会翻成 false。
type AllowanceModel struct {
	// TotalCents / APICents / AutoCents 是反解出来的额度分母。
	TotalCents float64
	APICents   float64
	AutoCents  float64
	// SplitSolved 为 false 表示只解出了总分母，API/auto 的拆分不可解。
	SplitSolved bool

	// *UsedCents 是对应的已消耗量。
	TotalUsedCents float64
	APIUsedCents   float64
	AutoUsedCents  float64

	// DerivedPercent 是用反解分母重算的总占比，应当与服务端的
	// totalPercentUsed 一致（容差内）。
	DerivedPercent float64

	// Consistent 表示反解结果自洽。false 说明 Cursor 可能改了计费模型，
	// 这时 Notes 会写明是哪一项对不上。
	Consistent bool
	Notes      []string
}

// 反解校验的容差。金额都是整数分，正常情况下误差在 1e-9 量级。
const (
	allowanceRelTol = 1e-6
	allowanceIntTol = 1e-3
)

func nearlyEqual(a, b float64) bool {
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return math.Abs(a-b) <= allowanceRelTol*scale
}

func nearlyInteger(v float64) bool {
	return math.Abs(v-math.Round(v)) <= allowanceIntTol
}

// deriveAllowance 从已解析出的百分比与 breakdown.total 反解额度口径。
// 条件不足时返回 nil；能解出总分母但拆不出 API/auto 时返回 SplitSolved=false。
func deriveAllowance(s *Summary) *AllowanceModel {
	if s.PlanBreakdownTotalCents == nil || s.TotalPercentUsed == nil {
		return nil
	}
	total := *s.PlanBreakdownTotalCents
	totalPct := *s.TotalPercentUsed
	if totalPct <= 0 || total <= 0 {
		return nil
	}

	m := &AllowanceModel{
		TotalCents:     total / (totalPct / 100),
		TotalUsedCents: total,
	}
	m.DerivedPercent = total / m.TotalCents * 100

	if !nearlyInteger(m.TotalCents) {
		m.Notes = append(m.Notes,
			fmt.Sprintf("总分母 %.4f 不是整数分，计费模型可能已变更", m.TotalCents))
	}

	if s.APIPercentUsed == nil || s.AutoPercentUsed == nil {
		m.Notes = append(m.Notes, "缺少 apiPercentUsed / autoPercentUsed，无法拆分 API 与 auto")
		m.Consistent = len(m.Notes) == 1 && nearlyInteger(m.TotalCents)
		return m
	}

	a, b := *s.APIPercentUsed/100, *s.AutoPercentUsed/100
	if math.Abs(a-b) < allowanceRelTol {
		// 两个百分比相等时方程退化，解不出唯一拆分。
		m.Notes = append(m.Notes, "apiPercentUsed 与 autoPercentUsed 相等，拆分方程退化")
		m.Consistent = nearlyInteger(m.TotalCents)
		return m
	}

	m.APICents = (total - b*m.TotalCents) / (a - b)
	m.AutoCents = m.TotalCents - m.APICents
	m.APIUsedCents = a * m.APICents
	m.AutoUsedCents = b * m.AutoCents
	m.SplitSolved = true

	if m.APICents <= 0 || m.AutoCents <= 0 {
		m.Notes = append(m.Notes,
			fmt.Sprintf("反解出的分母为负或零（API %.2f / auto %.2f）", m.APICents, m.AutoCents))
	}
	if !nearlyEqual(m.APIUsedCents+m.AutoUsedCents, total) {
		m.Notes = append(m.Notes,
			fmt.Sprintf("两路用量相加 %.4f 与 breakdown.total %.0f 不符", m.APIUsedCents+m.AutoUsedCents, total))
	}
	if !nearlyEqual(m.APICents+m.AutoCents, m.TotalCents) {
		m.Notes = append(m.Notes,
			fmt.Sprintf("两路分母相加 %.4f 与总分母 %.4f 不符", m.APICents+m.AutoCents, m.TotalCents))
	}
	if !nearlyEqual(m.DerivedPercent, totalPct) {
		m.Notes = append(m.Notes,
			fmt.Sprintf("重算占比 %.6f%% 与 totalPercentUsed %.6f%% 不符", m.DerivedPercent, totalPct))
	}
	// 真正的独立证据：金额必须落在整数分上。用有序 slice 保证 Notes 顺序稳定。
	for _, chk := range []struct {
		label string
		value float64
	}{
		{"API 分母", m.APICents},
		{"auto 分母", m.AutoCents},
		{"API 用量", m.APIUsedCents},
		{"auto 用量", m.AutoUsedCents},
	} {
		if !nearlyInteger(chk.value) {
			m.Notes = append(m.Notes, fmt.Sprintf("%s %.4f 不是整数分", chk.label, chk.value))
		}
	}

	m.Consistent = len(m.Notes) == 0
	return m
}

// derivePercent 用 used/limit 自算占比，但只在 limit 确实是真实分母时才给结果。
//
// 实测（enterprise/team 账号）响应形如：
//
//	plan.used = 2000, plan.limit = 2000, plan.remaining = 0
//	plan.breakdown = {included: 2000, bonus: 3435, total: 5435}
//	plan.totalPercentUsed = 6.038888888888889
//
// 这里 limit 只覆盖 included 额度，used 已经把它用满，所以 used/limit 恒为
// 100%——纯属误导。breakdown.total 也不能拿来当分母：它是**已消耗用量**
// （included + bonus）而不是额度上限，用它算出来是 36.8%，同样对不上。
//
// 真实关系是 totalPercentUsed == breakdown.total / 900，即真实分母为
// 90000 分（$900）。这个数没有任何字段直接给出，但可以由 breakdown.total
// 与三个百分比反解出来（推导过程见 AllowanceModel），拆分为
// API $200 + auto $700。三次实测全部精确吻合。
//
// **即便如此，这里依然不能把 90000 硬编码进来**，原因有两条：
//  1. 循环依赖：反解分母必须先有 totalPercentUsed。而 DerivedPercent 存在的
//     意义恰恰是「服务端没给 totalPercentUsed 时的兜底」——那种场景下分母
//     同样推不出来。
//  2. $200/$700 这个拆分是否随账号套餐变化，目前只有一个账号的观测，未知。
//
// 所以策略不变：只有在没有 bonus 额度外溢的迹象时（没有 breakdown，或
// breakdown.total 未超过 limit）才认为 limit 是完整分母；否则返回 nil，
// 由调用方显示为「—」，宁可留空也不给误导性数字。
func derivePercent(used, limit, breakdownTotal *float64) *float64 {
	if used == nil || limit == nil || *limit <= 0 {
		return nil
	}
	if breakdownTotal != nil && *breakdownTotal > *limit {
		return nil
	}
	percent := (*used / *limit) * 100
	return &percent
}

func (s *Summary) note(found bool, foundLabel, missingLabel string) {
	if found {
		s.FoundPaths = append(s.FoundPaths, foundLabel)
	} else {
		s.MissingPaths = append(s.MissingPaths, missingLabel)
	}
}

// OnDemandView 是 On-Demand 额度的展示态，对应前端 getCursorOnDemandSummary。
//
// HasFixedLimit / IsTeamManagedLimit / IsUnlimited / IsDisabled 四者互斥，
// 恰好覆盖全部情况，调用方按这个顺序判断即可。
type OnDemandView struct {
	IsTeamLimit bool
	UsedCents   float64
	LimitCents  *float64
	// Enabled 直接来自服务端的 enabled 字段；nil 表示响应里没有这个字段。
	Enabled *bool
	// HasFixedLimit：已开启且有个人固定上限。
	HasFixedLimit bool
	// IsTeamManagedLimit：已开启，但没有个人固定上限，额度上限由团队侧管理。
	IsTeamManagedLimit bool
	// IsUnlimited：已开启且不设上限。
	IsUnlimited bool
	// IsDisabled：未开启。
	IsDisabled bool

	// 以下字段如实透出两侧的原始值，供展示层同时呈现。
	// nil 表示响应里**没有这个字段**，与「值是 0」是两回事，不要混淆。
	// 它们不参与任何状态判断，只是让取值过程对使用者透明。
	IndividualUsedCents  *float64
	IndividualLimitCents *float64
	TeamUsedCents        *float64
	TeamLimitCents       *float64
	// UsedSource / LimitSource 说明 UsedCents / LimitCents 实际取自哪一侧。
	UsedSource  string
	LimitSource string
}

// 额度取值来源标记，用于让 OnDemandView 的取值过程可解释。
const (
	SideIndividual = "个人侧"
	SideTeam       = "团队侧"
	SideNone       = "无"
)

// resolveSide 复刻 OnDemand() 里 firstNonNil(team, individual) 的选择结果，
// 用于向使用者说明数值究竟取自哪一侧。
func resolveSide(team, individual *float64, preferTeam bool) string {
	if preferTeam && team != nil {
		return SideTeam
	}
	if individual != nil {
		return SideIndividual
	}
	return SideNone
}

// OnDemand 计算 On-Demand 展示态。
//
// 判断原则：enabled 是服务端直接给出的事实，优先采信；limit 只决定「上限是
// 多少」，不能拿来推断「开没开」。旧实现从 limit 反推启用状态，导致
// 「enabled=true + limitType=team + limit=null」这种真实存在的组合被误判成
// 「未开启」——team 账号的上限本来就由团队侧管理，个人侧不返回 limit。
func (s *Summary) OnDemand() OnDemandView {
	view := OnDemandView{
		IsTeamLimit: s.OnDemandLimitType == "team",
		Enabled:     s.OnDemandEnabled,

		IndividualUsedCents:  s.OnDemandUsedCents,
		IndividualLimitCents: s.OnDemandLimitCents,
		TeamUsedCents:        s.TeamOnDemandUsedCents,
		TeamLimitCents:       s.TeamOnDemandLimitCents,
	}
	view.UsedSource = resolveSide(s.TeamOnDemandUsedCents, s.OnDemandUsedCents, view.IsTeamLimit)
	view.LimitSource = resolveSide(s.TeamOnDemandLimitCents, s.OnDemandLimitCents, view.IsTeamLimit)

	used := s.OnDemandUsedCents
	limit := s.OnDemandLimitCents
	if view.IsTeamLimit {
		// team 账号必须锁定在同一个额度口径上，优先团队字段。
		used = firstNonNil(s.TeamOnDemandUsedCents, s.OnDemandUsedCents)
		limit = firstNonNil(s.TeamOnDemandLimitCents, s.OnDemandLimitCents)
	}
	if used != nil {
		view.UsedCents = *used
	}
	view.LimitCents = limit
	view.HasFixedLimit = limit != nil && *limit > 0

	switch {
	case s.OnDemandEnabled != nil && !*s.OnDemandEnabled:
		// 服务端明确说没开，即使残留着 limit 也以此为准。
		view.IsDisabled = true
		view.HasFixedLimit = false
	case s.OnDemandEnabled != nil:
		// 明确开启，接下来只需要区分上限口径。
	default:
		// enabled 字段缺失，无从判断，退回旧的推断方式：有固定上限才算开启。
		view.IsDisabled = !view.HasFixedLimit
	}

	if !view.IsDisabled && !view.HasFixedLimit {
		if view.IsTeamLimit {
			view.IsTeamManagedLimit = true
		} else {
			view.IsUnlimited = true
		}
	}
	return view
}

// FormatCents 把美分转成 $x.xx。
func FormatCents(cents *float64) string {
	if cents == nil {
		return "—"
	}
	return fmt.Sprintf("$%.2f", *cents/100)
}

// FormatPercent 格式化百分比。
func FormatPercent(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", *v)
}

// ---------------------------------------------------------------------------
// 取值辅助
// ---------------------------------------------------------------------------

func firstPath(root map[string]any, candidates ...[]string) (any, string) {
	for _, keys := range candidates {
		if v := getPath(root, keys...); v != nil {
			return v, strings.Join(keys, ".")
		}
	}
	return nil, ""
}

func getPath(root any, keys ...string) any {
	cur := root
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
		if cur == nil {
			return nil
		}
	}
	return cur
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// pickNumber 支持 JSON number、json.Number 与数字字符串三种形态。
func pickNumber(obj any, keys ...string) *float64 {
	m := asMap(obj)
	if m == nil {
		return nil
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case float64:
			if !math.IsNaN(t) && !math.IsInf(t, 0) {
				out := t
				return &out
			}
		case json.Number:
			if f, err := t.Float64(); err == nil {
				return &f
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return &f
			}
		case bool:
			continue
		}
	}
	return nil
}

func pickBool(obj any, keys ...string) *bool {
	m := asMap(obj)
	if m == nil {
		return nil
	}
	for _, k := range keys {
		switch t := m[k].(type) {
		case bool:
			out := t
			return &out
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true":
				out := true
				return &out
			case "false":
				out := false
				return &out
			}
		}
	}
	return nil
}

func pickString(obj any, keys ...string) string {
	m := asMap(obj)
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func boolValue(obj any, key string) bool {
	m := asMap(obj)
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

func firstNonNil(values ...*float64) *float64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "<无>"
	}
	return s
}
