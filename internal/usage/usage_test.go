package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"testing"
)

// parseJSON 把测试用的 JSON 字面量喂给 Parse，模拟真实的 usage-summary 响应。
func parseJSON(t *testing.T, raw string) *Summary {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("测试数据不是合法 JSON: %v", err)
	}
	return Parse(m)
}

func wantFloat(t *testing.T, label string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: 期望 %v，实际为 nil", label, want)
	}
	if math.Abs(*got-want) > 1e-6 {
		t.Errorf("%s: 期望 %v，实际 %v", label, want, *got)
	}
}

func wantNil(t *testing.T, label string, got *float64) {
	t.Helper()
	if got != nil {
		t.Errorf("%s: 期望 nil，实际 %v", label, *got)
	}
}

// 实测的 enterprise/team 账号响应（数值取自 2026-08-16 的真实抓取）。
const realWorldTeamResponse = `{
  "membershipType": "enterprise",
  "limitType": "team",
  "isUnlimited": false,
  "billingCycleEnd": "2026-09-16T07:31:51.000Z",
  "individualUsage": {
    "plan": {
      "enabled": true,
      "used": 2000,
      "limit": 2000,
      "remaining": 0,
      "totalPercentUsed": 6.038888888888889,
      "autoPercentUsed": 0.6928571428571428,
      "apiPercentUsed": 24.75,
      "breakdown": {"included": 2000, "bonus": 3435, "total": 5435}
    },
    "onDemand": {"enabled": true, "limit": null, "remaining": null, "used": 0}
  },
  "teamUsage": {
    "onDemand": {"enabled": true, "limit": null, "remaining": null, "used": 0}
  }
}`

// 同一份数据的 snake_case 写法，用于验证双路径兼容。
const realWorldTeamResponseSnake = `{
  "membership_type": "enterprise",
  "limit_type": "team",
  "is_unlimited": false,
  "billing_cycle_end": "2026-09-16T07:31:51.000Z",
  "individual_usage": {
    "plan": {
      "enabled": true,
      "used": 2000,
      "limit": 2000,
      "remaining": 0,
      "total_percent_used": 6.038888888888889,
      "auto_percent_used": 0.6928571428571428,
      "api_percent_used": 24.75,
      "break_down": {"included": 2000, "bonus": 3435, "total": 5435}
    },
    "on_demand": {"enabled": true, "limit": null, "remaining": null, "used": 0}
  },
  "team_usage": {
    "on_demand": {"enabled": true, "limit": null, "remaining": null, "used": 0}
  }
}`

// TestParseCamelAndSnakeAgree 覆盖需求 1：两种 key 风格必须解析出相同结果。
// 在此之前 snake_case 分支从未被执行过（真实响应全是 camelCase）。
func TestParseCamelAndSnakeAgree(t *testing.T) {
	camel := parseJSON(t, realWorldTeamResponse)
	snake := parseJSON(t, realWorldTeamResponseSnake)

	if camel.MembershipType != snake.MembershipType {
		t.Errorf("membershipType 不一致: %q vs %q", camel.MembershipType, snake.MembershipType)
	}
	if camel.Badge != snake.Badge {
		t.Errorf("Badge 不一致: %q vs %q", camel.Badge, snake.Badge)
	}
	if camel.OnDemandLimitType != snake.OnDemandLimitType {
		t.Errorf("limitType 不一致: %q vs %q", camel.OnDemandLimitType, snake.OnDemandLimitType)
	}

	for _, pair := range []struct {
		label        string
		camel, snake *float64
	}{
		{"TotalPercentUsed", camel.TotalPercentUsed, snake.TotalPercentUsed},
		{"AutoPercentUsed", camel.AutoPercentUsed, snake.AutoPercentUsed},
		{"APIPercentUsed", camel.APIPercentUsed, snake.APIPercentUsed},
		{"PlanUsedCents", camel.PlanUsedCents, snake.PlanUsedCents},
		{"PlanLimitCents", camel.PlanLimitCents, snake.PlanLimitCents},
		{"PlanBreakdownTotalCents", camel.PlanBreakdownTotalCents, snake.PlanBreakdownTotalCents},
		{"EffectivePercent", camel.EffectivePercent, snake.EffectivePercent},
	} {
		switch {
		case pair.camel == nil && pair.snake == nil:
		case pair.camel == nil || pair.snake == nil:
			t.Errorf("%s: 一侧为 nil（camel=%v snake=%v）", pair.label, pair.camel, pair.snake)
		case math.Abs(*pair.camel-*pair.snake) > 1e-9:
			t.Errorf("%s: 不一致 %v vs %v", pair.label, *pair.camel, *pair.snake)
		}
	}

	if camel.AllowanceResetAt == nil || snake.AllowanceResetAt == nil {
		t.Fatal("billingCycleEnd 应当在两种风格下都被解析出来")
	}
	if *camel.AllowanceResetAt != *snake.AllowanceResetAt {
		t.Errorf("AllowanceResetAt 不一致: %d vs %d", *camel.AllowanceResetAt, *snake.AllowanceResetAt)
	}

	// 不能直接用 == 比较：Enabled 是指针，比的会是地址而不是值。
	cv, sv := camel.OnDemand(), snake.OnDemand()
	if boolView(cv) != boolView(sv) {
		t.Errorf("OnDemandView 不一致:\n camel=%s\n snake=%s", boolView(cv), boolView(sv))
	}
}

// boolView 把 OnDemandView 摊平成可比较的字符串，避免指针字段干扰比较。
func boolView(v OnDemandView) string {
	enabled := "nil"
	if v.Enabled != nil {
		enabled = strconv.FormatBool(*v.Enabled)
	}
	limit := "nil"
	if v.LimitCents != nil {
		limit = strconv.FormatFloat(*v.LimitCents, 'f', -1, 64)
	}
	return fmt.Sprintf(
		"team=%v used=%v limit=%s enabled=%s fixed=%v teamManaged=%v unlimited=%v disabled=%v",
		v.IsTeamLimit, v.UsedCents, limit, enabled,
		v.HasFixedLimit, v.IsTeamManagedLimit, v.IsUnlimited, v.IsDisabled)
}

// TestDerivedPercentNotBogus100 覆盖需求 2（Bug 1）：
// limit=2000 但 breakdown.total=5435，说明 bonus 额度在 limit 之外，
// used/limit 恒为 100% 属于误导，必须不产出这个数字。
func TestDerivedPercentNotBogus100(t *testing.T) {
	s := parseJSON(t, realWorldTeamResponse)

	if s.DerivedPercent != nil && math.Abs(*s.DerivedPercent-100) < 1e-9 {
		t.Fatalf("自算百分比又回到了误导性的 100%%（Bug 1 复发）")
	}
	wantNil(t, "DerivedPercent（limit 不是真实分母时应留空）", s.DerivedPercent)

	// 最终展示值必须来自服务端的 totalPercentUsed，约 6%。
	wantFloat(t, "EffectivePercent", s.EffectivePercent, 6.038888888888889)
	if *s.EffectivePercent > 10 {
		t.Errorf("EffectivePercent 应当接近 6%%，实际 %v", *s.EffectivePercent)
	}

	wantFloat(t, "breakdown.included", s.PlanBreakdownIncludedCents, 2000)
	wantFloat(t, "breakdown.bonus", s.PlanBreakdownBonusCents, 3435)
	wantFloat(t, "breakdown.total", s.PlanBreakdownTotalCents, 5435)
}

// TestDerivedPercentWithoutBreakdown 确认没有 breakdown 时 used/limit 仍然可用。
func TestDerivedPercentWithoutBreakdown(t *testing.T) {
	s := parseJSON(t, `{
	  "individualUsage": {"plan": {"used": 500, "limit": 2000}}
	}`)
	wantFloat(t, "DerivedPercent", s.DerivedPercent, 25)
	wantFloat(t, "EffectivePercent", s.EffectivePercent, 25)
}

// TestDerivedPercentBreakdownWithinLimit 确认 breakdown.total 未超过 limit 时
// （没有 bonus 外溢）limit 仍是有效分母。
func TestDerivedPercentBreakdownWithinLimit(t *testing.T) {
	s := parseJSON(t, `{
	  "individualUsage": {"plan": {
	    "used": 500, "limit": 2000,
	    "breakdown": {"included": 500, "bonus": 0, "total": 500}
	  }}
	}`)
	wantFloat(t, "DerivedPercent", s.DerivedPercent, 25)
}

// TestOnDemandTeamManagedNotDisabled 覆盖需求 3（Bug 2）：
// enabled=true + limitType=team + limit=null 绝不能被判成「未开启」。
func TestOnDemandTeamManagedNotDisabled(t *testing.T) {
	s := parseJSON(t, realWorldTeamResponse)
	view := s.OnDemand()

	if view.IsDisabled {
		t.Fatal("enabled=true 却被判成「未开启」（Bug 2 复发）")
	}
	if !view.IsTeamLimit {
		t.Error("limitType=team 应当识别为团队额度口径")
	}
	if !view.IsTeamManagedLimit {
		t.Error("limit=null 的 team 账号应当落在「上限由团队侧管理」这个状态")
	}
	if view.HasFixedLimit {
		t.Error("limit 为 null，不应认为有固定上限")
	}
	if view.IsUnlimited {
		t.Error("team 账号不应被标成「无限」，上限只是不在个人侧")
	}
	if view.Enabled == nil || !*view.Enabled {
		t.Error("Enabled 应当如实反映服务端的 enabled=true")
	}
	assertExactlyOneState(t, view)
}

// TestOnDemandBothSidesExposed 断言个人侧与团队侧的数值都能如实透出，
// 并且「字段缺失(nil)」与「值为 0」可以区分——这两者混淆会误导排查。
func TestOnDemandBothSidesExposed(t *testing.T) {
	// 真实响应：个人侧 used=0（字段存在且为 0），团队侧 used=447，两侧都没有 limit。
	view := parseJSON(t, `{
	  "limitType": "team",
	  "individualUsage": {"onDemand": {"enabled": true, "limit": null, "used": 0}},
	  "teamUsage": {"onDemand": {"enabled": true, "limit": null, "used": 447}}
	}`).OnDemand()

	if view.IndividualUsedCents == nil {
		t.Fatal("个人侧 used=0 是存在的字段，不该是 nil")
	}
	if *view.IndividualUsedCents != 0 {
		t.Errorf("个人侧 used 期望 0，实际 %v", *view.IndividualUsedCents)
	}
	if view.TeamUsedCents == nil || *view.TeamUsedCents != 447 {
		t.Errorf("团队侧 used 期望 447，实际 %v", view.TeamUsedCents)
	}
	if view.IndividualLimitCents != nil {
		t.Errorf("个人侧 limit 是 null，应当为 nil，实际 %v", *view.IndividualLimitCents)
	}
	if view.TeamLimitCents != nil {
		t.Errorf("团队侧 limit 是 null，应当为 nil，实际 %v", *view.TeamLimitCents)
	}

	// nil 与 0 的展示必须不同。
	if FormatCents(view.IndividualUsedCents) != "$0.00" {
		t.Errorf("值为 0 应显示 $0.00，实际 %q", FormatCents(view.IndividualUsedCents))
	}
	if FormatCents(view.IndividualLimitCents) != "—" {
		t.Errorf("字段缺失应显示 —，实际 %q", FormatCents(view.IndividualLimitCents))
	}

	// 取值来源必须如实说明，且与 UsedCents 的实际取值一致。
	if view.UsedSource != SideTeam {
		t.Errorf("limitType=team 且团队侧有值，UsedSource 期望 %q，实际 %q", SideTeam, view.UsedSource)
	}
	if view.UsedCents != 447 {
		t.Errorf("采用团队侧后 UsedCents 期望 447，实际 %v", view.UsedCents)
	}
	if view.LimitSource != SideNone {
		t.Errorf("两侧都没有 limit，LimitSource 期望 %q，实际 %q", SideNone, view.LimitSource)
	}
}

// TestOnDemandSourceAttribution 覆盖取值来源在各种组合下的归属。
func TestOnDemandSourceAttribution(t *testing.T) {
	cases := []struct {
		name           string
		raw            string
		wantUsedSource string
		wantUsedCents  float64
	}{
		{
			name:           "非 team：只看个人侧，团队侧再大也不采用",
			raw:            `{"individualUsage":{"onDemand":{"used":10}},"teamUsage":{"onDemand":{"used":999}}}`,
			wantUsedSource: SideIndividual,
			wantUsedCents:  10,
		},
		{
			name:           "team：优先团队侧",
			raw:            `{"limitType":"team","individualUsage":{"onDemand":{"used":10}},"teamUsage":{"onDemand":{"used":999}}}`,
			wantUsedSource: SideTeam,
			wantUsedCents:  999,
		},
		{
			name:           "team 但团队侧缺失：回退个人侧",
			raw:            `{"limitType":"team","individualUsage":{"onDemand":{"used":10}}}`,
			wantUsedSource: SideIndividual,
			wantUsedCents:  10,
		},
		{
			name:           "两侧都缺失",
			raw:            `{"limitType":"team"}`,
			wantUsedSource: SideNone,
			wantUsedCents:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := parseJSON(t, tc.raw).OnDemand()
			if view.UsedSource != tc.wantUsedSource {
				t.Errorf("UsedSource 期望 %q，实际 %q", tc.wantUsedSource, view.UsedSource)
			}
			if view.UsedCents != tc.wantUsedCents {
				t.Errorf("UsedCents 期望 %v，实际 %v", tc.wantUsedCents, view.UsedCents)
			}
		})
	}
}

// TestOnDemandStates 覆盖四种互斥状态与需求 4 的边界情况。
func TestOnDemandStates(t *testing.T) {
	cases := []struct {
		name               string
		raw                string
		wantDisabled       bool
		wantFixedLimit     bool
		wantTeamManaged    bool
		wantUnlimited      bool
		wantUsedCents      float64
		wantEnabledPresent bool
	}{
		{
			name:               "enabled=true 且有个人固定上限",
			raw:                `{"individualUsage":{"onDemand":{"enabled":true,"limit":5000,"used":1200}}}`,
			wantFixedLimit:     true,
			wantUsedCents:      1200,
			wantEnabledPresent: true,
		},
		{
			name:               "enabled=true 无上限且非 team → 无限",
			raw:                `{"individualUsage":{"onDemand":{"enabled":true,"limit":null,"used":300}}}`,
			wantUnlimited:      true,
			wantUsedCents:      300,
			wantEnabledPresent: true,
		},
		{
			name:               "enabled=true + team + limit=null → 团队侧管理",
			raw:                `{"limitType":"team","individualUsage":{"onDemand":{"enabled":true,"limit":null,"used":0}}}`,
			wantTeamManaged:    true,
			wantEnabledPresent: true,
		},
		{
			name:               "enabled=false 明确关闭，即使残留 limit 也以 enabled 为准",
			raw:                `{"individualUsage":{"onDemand":{"enabled":false,"limit":5000,"used":0}}}`,
			wantDisabled:       true,
			wantEnabledPresent: true,
		},
		{
			name:         "enabled 字段缺失且无上限 → 退回旧推断，判为未开启",
			raw:          `{"individualUsage":{"onDemand":{"limit":null,"used":0}}}`,
			wantDisabled: true,
		},
		{
			name:           "enabled 字段缺失但有上限 → 视为已开启",
			raw:            `{"individualUsage":{"onDemand":{"limit":4200,"used":100}}}`,
			wantFixedLimit: true,
			wantUsedCents:  100,
		},
		{
			name:         "limit=0 视为没有固定上限，且不触发除零",
			raw:          `{"individualUsage":{"onDemand":{"limit":0,"used":0}}}`,
			wantDisabled: true,
		},
		{
			name:          "limit=0 但 enabled=true → 无限而非未开启",
			raw:           `{"individualUsage":{"onDemand":{"enabled":true,"limit":0,"used":0}}}`,
			wantUnlimited: true,

			wantEnabledPresent: true,
		},
		{
			name:         "onDemand 整段缺失",
			raw:          `{"membershipType":"free"}`,
			wantDisabled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := parseJSON(t, tc.raw).OnDemand()

			if view.IsDisabled != tc.wantDisabled {
				t.Errorf("IsDisabled: 期望 %v，实际 %v", tc.wantDisabled, view.IsDisabled)
			}
			if view.HasFixedLimit != tc.wantFixedLimit {
				t.Errorf("HasFixedLimit: 期望 %v，实际 %v", tc.wantFixedLimit, view.HasFixedLimit)
			}
			if view.IsTeamManagedLimit != tc.wantTeamManaged {
				t.Errorf("IsTeamManagedLimit: 期望 %v，实际 %v", tc.wantTeamManaged, view.IsTeamManagedLimit)
			}
			if view.IsUnlimited != tc.wantUnlimited {
				t.Errorf("IsUnlimited: 期望 %v，实际 %v", tc.wantUnlimited, view.IsUnlimited)
			}
			if view.UsedCents != tc.wantUsedCents {
				t.Errorf("UsedCents: 期望 %v，实际 %v", tc.wantUsedCents, view.UsedCents)
			}
			if (view.Enabled != nil) != tc.wantEnabledPresent {
				t.Errorf("Enabled 是否存在: 期望 %v，实际 %v", tc.wantEnabledPresent, view.Enabled != nil)
			}
			assertExactlyOneState(t, view)
		})
	}
}

// assertExactlyOneState 保证四个状态永远互斥且恰好命中一个，
// 这样调用方的 switch 不会漏分支。
func assertExactlyOneState(t *testing.T, view OnDemandView) {
	t.Helper()
	n := 0
	for _, on := range []bool{view.IsDisabled, view.HasFixedLimit, view.IsTeamManagedLimit, view.IsUnlimited} {
		if on {
			n++
		}
	}
	if n != 1 {
		t.Errorf("四个状态应当恰好命中一个，实际命中 %d 个: %+v", n, view)
	}
}

// TestParseMissingPlanIsSafe 确认响应完全为空时不 panic、不产生假数据。
func TestParseMissingPlanIsSafe(t *testing.T) {
	s := parseJSON(t, `{}`)
	wantNil(t, "TotalPercentUsed", s.TotalPercentUsed)
	wantNil(t, "DerivedPercent", s.DerivedPercent)
	wantNil(t, "EffectivePercent", s.EffectivePercent)
	wantNil(t, "PlanBreakdownTotalCents", s.PlanBreakdownTotalCents)
	if s.AllowanceResetAt != nil {
		t.Error("没有 billingCycleEnd 时不应产出重置时间")
	}
	if s.Badge != BadgeUnknown {
		t.Errorf("空响应的徽章应为 UNKNOWN，实际 %q", s.Badge)
	}
}

// TestSpendLimitUsageFallback 覆盖此前从未执行过的 spendLimitUsage 分支。
func TestSpendLimitUsageFallback(t *testing.T) {
	s := parseJSON(t, `{
	  "spendLimitUsage": {"used": 750, "limit": 3000, "limitType": "individual"}
	}`)
	wantFloat(t, "OnDemandUsedCents", s.OnDemandUsedCents, 750)
	wantFloat(t, "OnDemandLimitCents", s.OnDemandLimitCents, 3000)
	if s.OnDemandLimitType != "individual" {
		t.Errorf("limitType 应从 spendLimitUsage 兜底读出，实际 %q", s.OnDemandLimitType)
	}

	view := s.OnDemand()
	if !view.HasFixedLimit {
		t.Error("spendLimitUsage 提供了 limit=3000，应当算作有固定上限")
	}
	if view.UsedCents != 750 {
		t.Errorf("UsedCents 期望 750，实际 %v", view.UsedCents)
	}
}

// TestNumbersAsStrings 确认字符串形态的数字也能解析（pickNumber 的兼容分支）。
func TestNumbersAsStrings(t *testing.T) {
	s := parseJSON(t, `{
	  "individualUsage": {"plan": {"used": "500", "limit": "2000"}}
	}`)
	wantFloat(t, "PlanUsedCents", s.PlanUsedCents, 500)
	wantFloat(t, "PlanLimitCents", s.PlanLimitCents, 2000)
	wantFloat(t, "DerivedPercent", s.DerivedPercent, 25)
}

// TestDeriveAllowanceFromRealResponse 用真实响应验证额度分母的反解。
//
// 响应里没有任何字段直接给出分母，全靠 breakdown.total 与三个百分比解方程。
// 这里断言解出来的正是 API $200 + auto $700 = 总额 $900。
func TestDeriveAllowanceFromRealResponse(t *testing.T) {
	m := parseJSON(t, realWorldTeamResponse).Allowance
	if m == nil {
		t.Fatal("应当能从这份响应反解出额度口径")
	}
	if !m.SplitSolved {
		t.Fatal("API/auto 拆分应当可解")
	}

	for _, c := range []struct {
		label string
		got   float64
		want  float64
	}{
		{"总分母", m.TotalCents, 90000},
		{"API 分母", m.APICents, 20000},
		{"auto 分母", m.AutoCents, 70000},
		{"总用量", m.TotalUsedCents, 5435},
		{"API 用量", m.APIUsedCents, 4950},
		{"auto 用量", m.AutoUsedCents, 485},
	} {
		if math.Abs(c.got-c.want) > 1e-6 {
			t.Errorf("%s: 期望 %v，实际 %v", c.label, c.want, c.got)
		}
	}

	// 两路相加必须等于 breakdown.total。
	if sum := m.APIUsedCents + m.AutoUsedCents; math.Abs(sum-5435) > 1e-6 {
		t.Errorf("两路用量相加期望 5435，实际 %v", sum)
	}
	if sum := m.APICents + m.AutoCents; math.Abs(sum-90000) > 1e-6 {
		t.Errorf("两路分母相加期望 90000，实际 %v", sum)
	}
	// 重算占比必须与服务端 totalPercentUsed 在容差内一致。
	if math.Abs(m.DerivedPercent-6.038888888888889) > 1e-9 {
		t.Errorf("重算占比期望 6.038888888888889，实际 %v", m.DerivedPercent)
	}
	if !m.Consistent {
		t.Errorf("这份响应应当判定为闭合，实际 Notes=%v", m.Notes)
	}
}

// TestDeriveAllowanceAllThreeCaptures 用三次真实抓取交叉验证：
// 分母恒为 20000/70000，而用量各不相同，说明反解不是对单点数据的过拟合。
func TestDeriveAllowanceAllThreeCaptures(t *testing.T) {
	captures := []struct {
		name                     string
		apiPct, autoPct, totlPct float64
		breakdownTotal           float64
		wantAPIUsed, wantAutoUsd float64
	}{
		{"run1", 21.025, 0.6928571428571428, 5.21111111111111, 4690, 4205, 485},
		{"run2", 24.75, 0.6928571428571428, 6.038888888888889, 5435, 4950, 485},
		{"run3", 33.535, 0.6928571428571428, 7.9911111111111115, 7192, 6707, 485},
	}

	for _, c := range captures {
		t.Run(c.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"individualUsage":{"plan":{
			  "used":2000,"limit":2000,
			  "apiPercentUsed":%v,"autoPercentUsed":%v,"totalPercentUsed":%v,
			  "breakdown":{"included":2000,"bonus":%v,"total":%v}}}}`,
				c.apiPct, c.autoPct, c.totlPct, c.breakdownTotal-2000, c.breakdownTotal)

			m := parseJSON(t, raw).Allowance
			if m == nil || !m.SplitSolved {
				t.Fatalf("反解失败: %+v", m)
			}
			if math.Abs(m.TotalCents-90000) > 1e-3 {
				t.Errorf("总分母期望 90000，实际 %v", m.TotalCents)
			}
			if math.Abs(m.APICents-20000) > 1e-3 {
				t.Errorf("API 分母期望 20000，实际 %v", m.APICents)
			}
			if math.Abs(m.AutoCents-70000) > 1e-3 {
				t.Errorf("auto 分母期望 70000，实际 %v", m.AutoCents)
			}
			if math.Abs(m.APIUsedCents-c.wantAPIUsed) > 1e-3 {
				t.Errorf("API 用量期望 %v，实际 %v", c.wantAPIUsed, m.APIUsedCents)
			}
			if math.Abs(m.AutoUsedCents-c.wantAutoUsd) > 1e-3 {
				t.Errorf("auto 用量期望 %v，实际 %v", c.wantAutoUsd, m.AutoUsedCents)
			}
			if !m.Consistent {
				t.Errorf("应当判定为闭合，实际 Notes=%v", m.Notes)
			}
		})
	}
}

// TestDeriveAllowanceDetectsInconsistency 构造一份「对不上」的响应，
// 确认校验逻辑会报警而不是默默给出脏数字。
//
// 这里把 totalPercentUsed 调成与 api/auto 两路不相容的值：解方程仍能得到
// 数值解，但解出来的分母不再是整数分，Consistent 必须翻成 false。
func TestDeriveAllowanceDetectsInconsistency(t *testing.T) {
	m := parseJSON(t, `{"individualUsage":{"plan":{
	  "apiPercentUsed":33.535,"autoPercentUsed":0.6928571428571428,
	  "totalPercentUsed":11.1111,
	  "breakdown":{"included":2000,"bonus":5192,"total":7192}}}}`).Allowance

	if m == nil {
		t.Fatal("即使不闭合也应当返回模型，以便展示问题")
	}
	if m.Consistent {
		t.Errorf("这份数据不该判定为闭合（总分母 %.4f，API %.4f，auto %.4f）",
			m.TotalCents, m.APICents, m.AutoCents)
	}
	if len(m.Notes) == 0 {
		t.Error("不闭合时必须给出说明")
	}
}

// TestDeriveAllowanceDegenerate 覆盖解不出来的几种情况。
func TestDeriveAllowanceDegenerate(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantNil  bool
		wantSolv bool
	}{
		{
			name:    "缺 breakdown.total",
			raw:     `{"individualUsage":{"plan":{"totalPercentUsed":6.04}}}`,
			wantNil: true,
		},
		{
			name:    "缺 totalPercentUsed（正是 DerivedPercent 兜底的场景）",
			raw:     `{"individualUsage":{"plan":{"breakdown":{"total":7192}}}}`,
			wantNil: true,
		},
		{
			name:    "totalPercentUsed 为 0，除零保护",
			raw:     `{"individualUsage":{"plan":{"totalPercentUsed":0,"breakdown":{"total":7192}}}}`,
			wantNil: true,
		},
		{
			name:     "缺 api/auto，只能解出总分母",
			raw:      `{"individualUsage":{"plan":{"totalPercentUsed":7.9911111111111115,"breakdown":{"total":7192}}}}`,
			wantSolv: false,
		},
		{
			name: "api 与 auto 百分比相等，拆分方程退化",
			raw: `{"individualUsage":{"plan":{
			  "apiPercentUsed":5,"autoPercentUsed":5,
			  "totalPercentUsed":7.9911111111111115,
			  "breakdown":{"total":7192}}}}`,
			wantSolv: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := parseJSON(t, tc.raw).Allowance
			if tc.wantNil {
				if m != nil {
					t.Errorf("期望无法反解（nil），实际 %+v", m)
				}
				return
			}
			if m == nil {
				t.Fatal("期望能解出总分母，实际为 nil")
			}
			if m.SplitSolved != tc.wantSolv {
				t.Errorf("SplitSolved 期望 %v，实际 %v", tc.wantSolv, m.SplitSolved)
			}
			if math.Abs(m.TotalCents-90000) > 1e-3 {
				t.Errorf("总分母期望 90000，实际 %v", m.TotalCents)
			}
		})
	}
}

func TestBadgeNormalization(t *testing.T) {
	cases := map[string]PlanBadge{
		"free":        BadgeFree,
		"pro":         BadgePro,
		"pro_student": BadgePro,
		"pro_plus":    BadgeProPlus,
		"business":    BadgeEnterprise,
		"team":        BadgeEnterprise,
		"enterprise":  BadgeEnterprise,
		"free_trial":  BadgeFreeTrial,
		"ultra":       BadgeUltra,
		"":            BadgeUnknown,
	}
	for raw, want := range cases {
		if got := BadgeFromMembershipType(raw); got != want {
			t.Errorf("BadgeFromMembershipType(%q) = %q，期望 %q", raw, got, want)
		}
	}
}
