package procutil

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const exe = `D:\Program Files\cursor\Cursor.exe`

func rec(pid, ppid int, cmdline string) ProcessRecord {
	return ProcessRecord{PID: pid, ParentPID: ppid, ExePath: exe, CommandLine: cmdline}
}

// mainCmd 是主进程的命令行：没有 --type=，也没有 --user-data-dir。
func mainCmd() string { return `"` + exe + `"` }

func typedCmd(tag string) string {
	return `"` + exe + `" --type=` + tag +
		` --user-data-dir="C:\Users\gomal\AppData\Roaming\Cursor" --standard-schemes=vscode-webview`
}

// scriptCmd 是 Node 脚本子进程：**没有 --type=**，这正是踩过的坑。
func scriptCmd(script string) string {
	return `"` + exe + `" ` + script + ` --user-data-dir="C:\Users\gomal\AppData\Roaming\Cursor"`
}

// TestClassifyOnlyMain 只有主进程时的最简情形。
func TestClassifyOnlyMain(t *testing.T) {
	inv := Classify([]ProcessRecord{rec(1000, 500, mainCmd())})

	m, err := inv.SingleMain()
	if err != nil {
		t.Fatalf("应当识别出唯一主进程，实际报错：%v", err)
	}
	if m.PID != 1000 {
		t.Errorf("主进程 PID 期望 1000，实际 %d", m.PID)
	}
	if !strings.Contains(m.Reason, "不是") {
		t.Errorf("判定依据应说明父进程不是 Cursor.exe，实际 %q", m.Reason)
	}
}

// TestClassifyMainWithTypedChildren 主进程 + 各类 --type= 子进程。
func TestClassifyMainWithTypedChildren(t *testing.T) {
	records := []ProcessRecord{
		rec(1000, 500, mainCmd()),
		rec(1001, 1000, typedCmd("gpu-process")),
		rec(1002, 1000, typedCmd("utility")),
		rec(1003, 1000, typedCmd("renderer")),
		rec(1004, 1000, typedCmd("renderer")),
		rec(1005, 1000, typedCmd("crashpad-handler")),
	}
	inv := Classify(records)

	m, err := inv.SingleMain()
	if err != nil {
		t.Fatalf("应当识别出唯一主进程，实际报错：%v", err)
	}
	if m.PID != 1000 {
		t.Errorf("主进程 PID 期望 1000，实际 %d", m.PID)
	}
	for _, c := range inv.Processes {
		if c.PID == 1000 {
			continue
		}
		if c.Kind != KindTypedChild {
			t.Errorf("PID %d 应判为带 --type= 的子进程，实际 %s", c.PID, c.Kind)
		}
	}

	// 类别汇总：主进程排最前，renderer 有两个。
	cats := inv.Categories()
	if len(cats) == 0 || cats[0].Label != string(KindMain) {
		t.Fatalf("主进程应排在类别汇总最前，实际 %+v", cats)
	}
	for _, c := range cats {
		if c.Label == "--type=renderer" && len(c.PIDs) != 2 {
			t.Errorf("renderer 期望 2 个，实际 %d", len(c.PIDs))
		}
	}
}

// TestClassifyNodeScriptChildrenTrap 是真实踩过的坑：
// 有两个 Node 脚本子进程同样不带 --type=，只按「无 --type=」筛会一次命中 3 个。
func TestClassifyNodeScriptChildrenTrap(t *testing.T) {
	records := []ProcessRecord{
		rec(1000, 500, mainCmd()),
		rec(1001, 1000, typedCmd("gpu-process")),
		rec(1002, 1000, typedCmd("renderer")),
		rec(2001, 1000, scriptCmd(`"c:\Users\gomal\.cursor\extensions\ms-python\out\server.js"`)),
		rec(2002, 1000, scriptCmd(`"c:\Users\gomal\.cursor\extensions\eslint\server\out\eslintServer.js"`)),
	}

	// 先把坑本身固定下来：单看「无 --type=」会匹配 3 个，不止主进程。
	noType := 0
	for _, r := range records {
		if !strings.Contains(r.CommandLine, "--type=") {
			noType++
		}
	}
	if noType != 3 {
		t.Fatalf("用例前提有误：不带 --type= 的进程应有 3 个，实际 %d", noType)
	}

	inv := Classify(records)
	m, err := inv.SingleMain()
	if err != nil {
		t.Fatalf("补上「父进程不是 Cursor.exe」后应精确命中 1 个，实际报错：%v", err)
	}
	if m.PID != 1000 {
		t.Errorf("主进程 PID 期望 1000，实际 %d", m.PID)
	}

	for _, pid := range []int{2001, 2002} {
		var found bool
		for _, c := range inv.Processes {
			if c.PID != pid {
				continue
			}
			found = true
			if c.Kind != KindScriptChild {
				t.Errorf("PID %d 应判为 Node 脚本子进程，实际 %s", pid, c.Kind)
			}
			if !strings.Contains(c.Reason, "父进程") {
				t.Errorf("PID %d 的判定依据应提到父进程，实际 %q", pid, c.Reason)
			}
		}
		if !found {
			t.Errorf("PID %d 未出现在分类结果里", pid)
		}
	}
}

// TestUserDataDirFromChildrenOnly 固定另一条实测结论：
// --user-data-dir 只出现在子进程命令行里，主进程没有，只能反查。
func TestUserDataDirFromChildrenOnly(t *testing.T) {
	inv := Classify([]ProcessRecord{
		rec(1000, 500, mainCmd()),
		rec(1001, 1000, typedCmd("renderer")),
	})

	m, err := inv.SingleMain()
	if err != nil {
		t.Fatalf("SingleMain 失败：%v", err)
	}
	if m.UserDataDir != "" {
		t.Errorf("主进程命令行不该带 --user-data-dir，实际 %q", m.UserDataDir)
	}

	dirs := inv.UserDataDirs(1000)
	want := `C:\Users\gomal\AppData\Roaming\Cursor`
	if len(dirs) != 1 || dirs[0] != want {
		t.Errorf("应从子进程反查出 %q，实际 %v", want, dirs)
	}
}

// TestClassifyMultipleInstances 多实例时必须报错而不是随便挑一个。
func TestClassifyMultipleInstances(t *testing.T) {
	inv := Classify([]ProcessRecord{
		rec(1000, 500, mainCmd()),
		rec(1001, 1000, typedCmd("renderer")),
		rec(3000, 500, mainCmd()),
		rec(3001, 3000, typedCmd("renderer")),
	})

	if len(inv.Mains) != 2 {
		t.Fatalf("应识别出 2 个主进程，实际 %d", len(inv.Mains))
	}
	_, err := inv.SingleMain()
	if err == nil {
		t.Fatal("多实例时 SingleMain 必须报错，不能静默挑一个")
	}
	for _, want := range []string{"1000", "3000", "多实例"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际 %q", want, err.Error())
		}
	}
}

// TestClassifyNoProcess 没有任何 Cursor 进程。
func TestClassifyNoProcess(t *testing.T) {
	inv := Classify(nil)
	if len(inv.Processes) != 0 || len(inv.Mains) != 0 {
		t.Fatalf("空输入应得到空结果，实际 %+v", inv)
	}
	_, err := inv.SingleMain()
	if err == nil || !strings.Contains(err.Error(), "未检测到") {
		t.Errorf("应报「未检测到任何 Cursor.exe 进程」，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 三级查找
// ---------------------------------------------------------------------------

// providers 构造一组假的采集器，并统计 Stat 被调用了几次。
func providers(hits []RegistryHit, procs []ProcessRecord, defaults []string,
	exists map[string]bool, statCalls *int) Providers {
	return Providers{
		Registry:  func() ([]RegistryHit, error) { return hits, nil },
		Processes: func() ([]ProcessRecord, error) { return procs, nil },
		Defaults:  func() []string { return defaults },
		Stat: func(p string) bool {
			*statCalls++
			return exists[p]
		},
	}
}

func TestLocateTier1Registry(t *testing.T) {
	var statCalls int
	hits := []RegistryHit{{
		Root: "HKLM", View: "64 位", KeyName: "{D7D7...}_is1",
		DisplayName: "Cursor", DisplayVersion: "3.15.19",
		InstallLocation: `D:\Program Files\cursor`,
	}}
	loc, trace := Locate(providers(hits, []ProcessRecord{rec(1000, 500, mainCmd())}, nil, nil, &statCalls))

	if loc.Tier != TierRegistry {
		t.Errorf("应命中第 1 级注册表，实际 %s", loc.Tier)
	}
	if loc.Path != exe {
		t.Errorf("路径期望 %q，实际 %q", exe, loc.Path)
	}
	// 关键：exe 在坏盘上，有进程佐证时绝不能去 stat。
	if statCalls != 0 {
		t.Errorf("有进程正在运行该路径时不应访问磁盘，Stat 被调用了 %d 次", statCalls)
	}
	if loc.TouchedDisk {
		t.Error("TouchedDisk 应为 false")
	}
	if !strings.Contains(loc.Evidence, "PID 1000") {
		t.Errorf("存在性证据应指向运行中的进程，实际 %q", loc.Evidence)
	}
	if len(trace) == 0 {
		t.Error("应记录查找过程")
	}
}

// TestLocateRegistryFallsBackToStat 注册表给出的路径没有进程佐证时，才允许 stat。
func TestLocateRegistryFallsBackToStat(t *testing.T) {
	var statCalls int
	path := `C:\Program Files\cursor\Cursor.exe`
	hits := []RegistryHit{{Root: "HKCU", View: "64 位", KeyName: "x_is1",
		DisplayName: "Cursor", InstallLocation: `C:\Program Files\cursor`}}

	loc, _ := Locate(providers(hits, nil, nil, map[string]bool{path: true}, &statCalls))

	if loc.Tier != TierRegistry || loc.Path != path {
		t.Fatalf("应由注册表 + stat 命中 %q，实际 %s / %q", path, loc.Tier, loc.Path)
	}
	if statCalls != 1 {
		t.Errorf("应恰好 stat 一次，实际 %d 次", statCalls)
	}
	if !loc.TouchedDisk {
		t.Error("走了 stat，TouchedDisk 应为 true")
	}
}

// TestLocateTier2Process 注册表无结果时，退到运行中的进程。
func TestLocateTier2Process(t *testing.T) {
	var statCalls int
	procs := []ProcessRecord{
		rec(1001, 1000, typedCmd("renderer")),
		rec(1000, 500, mainCmd()),
	}
	loc, _ := Locate(providers(nil, procs, nil, nil, &statCalls))

	if loc.Tier != TierProcess {
		t.Fatalf("应命中第 2 级进程，实际 %s", loc.Tier)
	}
	if loc.Path != exe {
		t.Errorf("路径期望 %q，实际 %q", exe, loc.Path)
	}
	if !strings.Contains(loc.Detail, "1000") {
		t.Errorf("应优先取主进程 PID 1000，实际 %q", loc.Detail)
	}
	if statCalls != 0 {
		t.Errorf("第 2 级不该访问磁盘，Stat 被调用了 %d 次", statCalls)
	}
}

// TestLocateSkipsRegistryHitWithEmptyInstallLocation 注册表记录缺 InstallLocation 时跳过。
func TestLocateSkipsRegistryHitWithEmptyInstallLocation(t *testing.T) {
	var statCalls int
	hits := []RegistryHit{{Root: "HKLM", KeyName: "broken_is1", DisplayName: "Cursor"}}
	loc, _ := Locate(providers(hits, []ProcessRecord{rec(1000, 500, mainCmd())}, nil, nil, &statCalls))

	if loc.Tier != TierProcess {
		t.Errorf("注册表项不可用时应退到第 2 级，实际 %s", loc.Tier)
	}
}

// TestLocateTier3Default 前两级都落空，走默认路径兜底（含用户级与系统级）。
func TestLocateTier3Default(t *testing.T) {
	userLevel := `C:\Users\gomal\AppData\Local\Programs\Cursor\Cursor.exe`
	sysLevel := `C:\Program Files\cursor\Cursor.exe`

	for _, tc := range []struct {
		name   string
		exists map[string]bool
		want   string
	}{
		{"用户级默认位置命中", map[string]bool{userLevel: true}, userLevel},
		{"系统级默认位置命中", map[string]bool{sysLevel: true}, sysLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var statCalls int
			loc, _ := Locate(providers(nil, nil, []string{userLevel, sysLevel}, tc.exists, &statCalls))
			if loc.Tier != TierDefault {
				t.Fatalf("应命中第 3 级，实际 %s", loc.Tier)
			}
			if loc.Path != tc.want {
				t.Errorf("路径期望 %q，实际 %q", tc.want, loc.Path)
			}
		})
	}
}

// TestLocateAllMiss 三级全落空时必须返回空结果并留下痕迹，不能瞎编一个路径。
func TestLocateAllMiss(t *testing.T) {
	var statCalls int
	loc, trace := Locate(providers(nil, nil, []string{`C:\nope\Cursor.exe`}, nil, &statCalls))

	if loc.Path != "" {
		t.Errorf("全落空时路径应为空，实际 %q", loc.Path)
	}
	if !strings.Contains(strings.Join(trace, "\n"), "三级查找全部落空") {
		t.Errorf("应在查找过程里说明全部落空，实际 %v", trace)
	}
}

// TestLocateSurvivesProviderErrors 采集器报错时不能 panic，应记录并继续。
func TestLocateSurvivesProviderErrors(t *testing.T) {
	boom := errors.New("拒绝访问")
	loc, trace := Locate(Providers{
		Registry:  func() ([]RegistryHit, error) { return nil, boom },
		Processes: func() ([]ProcessRecord, error) { return nil, boom },
		Defaults:  func() []string { return nil },
		Stat:      func(string) bool { return false },
	})

	if loc.Path != "" {
		t.Errorf("全部失败时应返回空结果，实际 %q", loc.Path)
	}
	joined := strings.Join(trace, "\n")
	if !strings.Contains(joined, "拒绝访问") {
		t.Errorf("查找过程应记录采集失败原因，实际 %v", trace)
	}
}

// ---------------------------------------------------------------------------
// 关闭流程（全部使用假 Runner，绝不会真的执行 taskkill）
// ---------------------------------------------------------------------------

type fakeRunner struct{ calls [][]string }

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil
}

func fastOptions() StopOptions {
	return StopOptions{GraceTimeout: 30 * time.Millisecond, ForceTimeout: 30 * time.Millisecond,
		PollInterval: 5 * time.Millisecond}
}

// TestStopCursorGraceful 进程在优雅关闭后自行退出，不应升级为强杀。
func TestStopCursorGraceful(t *testing.T) {
	r := &fakeRunner{}
	calls := 0
	alive := func(context.Context, int) (bool, error) {
		calls++
		return calls <= 2, nil // 前两次还在，之后退出
	}

	report, err := StopCursor(context.Background(), 1000, fastOptions(), r, alive)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if !report.ExitedGracefully || report.Escalated {
		t.Errorf("应优雅退出且不升级，实际 %+v", report)
	}
	if len(r.calls) != 1 {
		t.Fatalf("应只执行 1 条命令，实际 %v", r.calls)
	}
	if strings.Contains(strings.Join(r.calls[0], " "), "/F") {
		t.Errorf("优雅关闭不得带 /F，实际 %v", r.calls[0])
	}
}

// TestStopCursorEscalates 优雅关闭超时后必须升级为 /T /F 强杀。
func TestStopCursorEscalates(t *testing.T) {
	r := &fakeRunner{}
	forced := false
	alive := func(context.Context, int) (bool, error) {
		// 只有在强杀命令发出之后才认为进程消失。
		return !forced, nil
	}
	runner := &recordingRunner{fake: r, onForce: func() { forced = true }}

	report, err := StopCursor(context.Background(), 1000, fastOptions(), runner, alive)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if !report.Escalated {
		t.Error("超时后应升级为强杀")
	}
	if report.ExitedGracefully {
		t.Error("被强杀了就不该标记为优雅退出")
	}
	if len(r.calls) != 2 {
		t.Fatalf("应执行 2 条命令（优雅 + 强杀），实际 %v", r.calls)
	}
	force := strings.Join(r.calls[1], " ")
	if !strings.Contains(force, "/T") || !strings.Contains(force, "/F") {
		t.Errorf("强杀命令应带 /T /F，实际 %q", force)
	}
}

// TestStopCursorAlreadyGone 进程本就不存在时，一条命令都不该发。
func TestStopCursorAlreadyGone(t *testing.T) {
	r := &fakeRunner{}
	alive := func(context.Context, int) (bool, error) { return false, nil }

	report, err := StopCursor(context.Background(), 1000, fastOptions(), r, alive)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if report.GracefulSent || len(r.calls) != 0 {
		t.Errorf("进程不存在时不该执行任何命令，实际 %v", r.calls)
	}
}

// recordingRunner 在强杀命令发出时触发回调，用于驱动存活状态变化。
type recordingRunner struct {
	fake    *fakeRunner
	onForce func()
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) error {
	err := r.fake.Run(ctx, name, args...)
	for _, a := range args {
		if a == "/F" {
			r.onForce()
		}
	}
	return err
}

// TestCommandShapes 固定命令形态：probe 第 7 段直接打印这些命令，
// 必须与 StopCursor 实际会执行的完全一致，否则「预演」就失去意义。
func TestCommandShapes(t *testing.T) {
	if got := strings.Join(GracefulKillCommand(42), " "); got != "taskkill /PID 42" {
		t.Errorf("优雅关闭命令不对：%q", got)
	}
	if got := strings.Join(ForceKillCommand(42), " "); got != "taskkill /PID 42 /T /F" {
		t.Errorf("强杀命令不对：%q", got)
	}
	if got := StartCommand(exe, "--new-window"); got[0] != exe || got[1] != "--new-window" {
		t.Errorf("启动命令不对：%v", got)
	}
}
