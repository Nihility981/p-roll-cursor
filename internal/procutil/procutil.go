// Package procutil 负责 Cursor 可执行文件的定位与进程识别，是写入式切号的前置能力。
//
// 本文件只放**纯逻辑**：不读注册表、不查 WMI、不碰磁盘、不依赖操作系统。
// 这样判定规则可以用构造好的进程记录直接做单元测试，不必真的启动 Cursor。
// 平台相关的采集在 discover_windows.go，关闭/启动在 control.go。
package procutil

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ExeName 是 Cursor 主程序的文件名。
const ExeName = "Cursor.exe"

// ProcessRecord 是一条进程快照，字段与 Win32_Process 对应。
type ProcessRecord struct {
	PID         int
	ParentPID   int
	ExePath     string
	CommandLine string
}

// ProcessKind 是进程分类结果。
type ProcessKind string

const (
	// KindMain 主进程：命令行无 --type=，且父进程不是 Cursor.exe。
	KindMain ProcessKind = "主进程"
	// KindTypedChild 带 --type= 的子进程（renderer / gpu-process / utility 等）。
	KindTypedChild ProcessKind = "带 --type= 的子进程"
	// KindScriptChild 无 --type= 但父进程是 Cursor.exe 的子进程（Node 脚本）。
	KindScriptChild ProcessKind = "Node 脚本子进程（无 --type=）"
)

// ClassifiedProcess 是带判定结论与依据的进程记录。
type ClassifiedProcess struct {
	ProcessRecord
	Kind ProcessKind
	// TypeTag 是 --type= 的取值，仅 KindTypedChild 非空。
	TypeTag string
	// UserDataDir 是 --user-data-dir= 的取值。注意：**主进程命令行里没有这个
	// 参数**，只有子进程才带，所以主进程这一项恒为空。
	UserDataDir string
	// Reason 说明为什么被判成这个类别，用于让判定过程对使用者可见。
	Reason string
}

// Inventory 是一次进程盘点的完整结果。
type Inventory struct {
	// Processes 按 PID 升序排列，保证多次运行的输出可以直接 diff。
	Processes []ClassifiedProcess
	// Mains 是识别出的主进程，正常情况下只有一个；多于一个说明开了多实例。
	Mains []ClassifiedProcess
}

// Classify 把一批 Cursor.exe 进程记录分类，并给出主进程判定。
//
// 判定主进程用两个条件，缺一不可：
//  1. 命令行里没有 --type=；
//  2. 父进程不是 Cursor.exe。
//
// 只用第 1 条是不够的——实测本机 17 个 Cursor 进程里，除主进程外还有两个
// Node 脚本子进程同样不带 --type=，只看这一条会一次命中 3 个。补上第 2 条
// 之后才精确命中 1 个。
func Classify(records []ProcessRecord) Inventory {
	pids := make(map[int]bool, len(records))
	for _, r := range records {
		pids[r.PID] = true
	}

	inv := Inventory{Processes: make([]ClassifiedProcess, 0, len(records))}
	for _, r := range records {
		c := ClassifiedProcess{
			ProcessRecord: r,
			TypeTag:       flagValue(r.CommandLine, "--type="),
			UserDataDir:   flagValue(r.CommandLine, "--user-data-dir="),
		}

		switch {
		case c.TypeTag != "":
			c.Kind = KindTypedChild
			c.Reason = fmt.Sprintf("命令行含 --type=%s", c.TypeTag)
		case pids[r.ParentPID]:
			c.Kind = KindScriptChild
			c.Reason = fmt.Sprintf("命令行无 --type=，但父进程 PID %d 也是主程序进程", r.ParentPID)
		default:
			c.Kind = KindMain
			c.Reason = fmt.Sprintf("命令行无 --type=，且父进程 PID %d 不是主程序进程", r.ParentPID)
		}

		inv.Processes = append(inv.Processes, c)
	}

	sort.Slice(inv.Processes, func(i, j int) bool {
		return inv.Processes[i].PID < inv.Processes[j].PID
	})
	for _, c := range inv.Processes {
		if c.Kind == KindMain {
			inv.Mains = append(inv.Mains, c)
		}
	}
	return inv
}

// SingleMain 返回唯一的主进程。没有进程或存在多实例时返回错误，
// 由调用方决定怎么处理——切号场景下多实例必须让用户先确认，不能瞎猜一个。
func (inv Inventory) SingleMain() (ClassifiedProcess, error) {
	switch len(inv.Mains) {
	case 1:
		return inv.Mains[0], nil
	case 0:
		if len(inv.Processes) == 0 {
			return ClassifiedProcess{}, fmt.Errorf("未检测到任何编辑器进程")
		}
		return ClassifiedProcess{}, fmt.Errorf("检测到 %d 个编辑器进程但没有一个符合主进程特征",
			len(inv.Processes))
	default:
		pids := make([]string, 0, len(inv.Mains))
		for _, m := range inv.Mains {
			pids = append(pids, fmt.Sprint(m.PID))
		}
		return ClassifiedProcess{}, fmt.Errorf("检测到 %d 个主进程（PID %s），疑似多实例",
			len(inv.Mains), strings.Join(pids, ", "))
	}
}

// UserDataDirs 反查某个主进程实际使用的 user-data-dir。
//
// 必须从子进程反查：主进程的命令行里没有 --user-data-dir，只有子进程带。
// 将来要按 user-data-dir 定位「某个特定实例」时，只能走这条路。
func (inv Inventory) UserDataDirs(mainPID int) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, c := range inv.Processes {
		if c.ParentPID != mainPID || c.UserDataDir == "" || seen[c.UserDataDir] {
			continue
		}
		seen[c.UserDataDir] = true
		dirs = append(dirs, c.UserDataDir)
	}
	sort.Strings(dirs)
	return dirs
}

// CategoryCount 是一类进程的汇总。
type CategoryCount struct {
	Label string
	PIDs  []int
}

// Categories 按类别汇总进程，主进程排在最前，其余按标签字典序，保证输出稳定。
func (inv Inventory) Categories() []CategoryCount {
	byLabel := map[string][]int{}
	for _, c := range inv.Processes {
		label := string(c.Kind)
		if c.Kind == KindTypedChild {
			label = "--type=" + c.TypeTag
		}
		byLabel[label] = append(byLabel[label], c.PID)
	}

	labels := make([]string, 0, len(byLabel))
	for l := range byLabel {
		labels = append(labels, l)
	}
	sort.Slice(labels, func(i, j int) bool {
		if a, b := labels[i] == string(KindMain), labels[j] == string(KindMain); a != b {
			return a
		}
		return labels[i] < labels[j]
	})

	out := make([]CategoryCount, 0, len(labels))
	for _, l := range labels {
		out = append(out, CategoryCount{Label: l, PIDs: byLabel[l]})
	}
	return out
}

// flagValue 从命令行里取出 name 后面的取值，支持带引号的写法。
// name 需要自带结尾的等号，例如 "--type="。
func flagValue(cmdline, name string) string {
	i := strings.Index(cmdline, name)
	if i < 0 {
		return ""
	}
	rest := cmdline[i+len(name):]
	if strings.HasPrefix(rest, `"`) {
		if end := strings.Index(rest[1:], `"`); end >= 0 {
			return rest[1 : 1+end]
		}
		return rest[1:]
	}
	if end := strings.IndexAny(rest, " \t"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// ---------------------------------------------------------------------------
// Cursor.exe 三级查找
// ---------------------------------------------------------------------------

// Tier 表示可执行文件是在第几级查找里命中的。
type Tier string

const (
	TierRegistry Tier = "注册表 Uninstall 的 InstallLocation"
	TierProcess  Tier = "运行中进程的 ExecutablePath"
	TierDefault  Tier = "默认安装路径兜底"
)

// RegistryHit 是一条 Uninstall 注册表记录。
type RegistryHit struct {
	Root            string // HKLM / HKCU
	View            string // 64 位 / 32 位(WOW6432Node)
	KeyName         string
	DisplayName     string
	DisplayVersion  string
	InstallLocation string
}

func (h RegistryHit) String() string {
	// 路径用 %s 而不是 %q：%q 会把反斜杠转义成双写，看起来像路径本身有问题。
	return fmt.Sprintf("%s\\...\\Uninstall\\%s [%s视图] DisplayName=%s DisplayVersion=%s InstallLocation=%s",
		h.Root, h.KeyName, h.View, orNone(h.DisplayName), orNone(h.DisplayVersion), orNone(h.InstallLocation))
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "<空>"
	}
	return s
}

// ExeLocation 是查找结果，连同「怎么找到的」「凭什么认为它存在」一起返回。
type ExeLocation struct {
	Path   string
	Tier   Tier
	Detail string
	// Evidence 说明存在性是怎么确认的。
	Evidence string
	// TouchedDisk 为 true 表示为了确认存在性真的访问了磁盘。
	TouchedDisk bool
}

// Providers 把三级查找所需的外部输入抽出来，便于单元测试注入假数据。
type Providers struct {
	Registry  func() ([]RegistryHit, error)
	Processes func() ([]ProcessRecord, error)
	Defaults  func() []string
	// Stat 判断路径是否存在。这是唯一会真正访问磁盘的入口。
	Stat func(string) bool
}

// Locate 按「注册表 → 运行中进程 → 默认路径」三级查找 Cursor.exe，
// 同时返回查找过程的逐步记录，便于在报告里说明命中了哪一级。
//
// 存在性验证的优先级是刻意设计的：**先看有没有进程正在运行这个路径，
// 实在没有才退回 os.Stat**。原因是本机 Cursor 装在 D 盘，而 D/E/F 是坏盘，
// 对它们做 os.Stat 有卡死风险；而「进程正在跑」本身就足以证明 exe 存在，
// 且注册表与 WMI 取到的都只是字符串，全程不碰那块盘。
func Locate(p Providers) (ExeLocation, []string) {
	var trace []string

	procs, err := p.Processes()
	if err != nil {
		trace = append(trace, fmt.Sprintf("进程枚举失败：%v", err))
	}
	inv := Classify(procs)
	// 小写路径 -> 佐证用的 PID。先填全部进程，再用主进程覆盖，
	// 这样报告里给出的证据会稳定地指向主进程，而不是随便一个 utility 子进程。
	running := map[string]int{}
	for _, r := range inv.Processes {
		if r.ExePath != "" {
			running[strings.ToLower(r.ExePath)] = r.PID
		}
	}
	for _, m := range inv.Mains {
		if m.ExePath != "" {
			running[strings.ToLower(m.ExePath)] = m.PID
		}
	}

	verify := func(path string) (string, bool, bool) {
		if pid, ok := running[strings.ToLower(path)]; ok {
			return fmt.Sprintf("PID %d 正在运行该路径（未访问磁盘）", pid), false, true
		}
		if p.Stat != nil && p.Stat(path) {
			return "os.Stat 确认存在（已访问磁盘）", true, true
		}
		return "", false, false
	}

	// 第 1 级：注册表。
	hits, err := p.Registry()
	if err != nil {
		trace = append(trace, fmt.Sprintf("[1] 注册表读取失败：%v", err))
	} else if len(hits) == 0 {
		trace = append(trace, "[1] 注册表：未找到编辑器的 Uninstall 记录")
	}
	for _, h := range hits {
		if strings.TrimSpace(h.InstallLocation) == "" {
			trace = append(trace, fmt.Sprintf("[1] 注册表命中但 InstallLocation 为空：%s", h))
			continue
		}
		path := filepath.Join(h.InstallLocation, ExeName)
		ev, disk, ok := verify(path)
		if !ok {
			trace = append(trace, fmt.Sprintf("[1] 注册表给出 %q，但无法确认存在", path))
			continue
		}
		trace = append(trace, fmt.Sprintf("[1] 注册表命中：%s", h))
		return ExeLocation{Path: path, Tier: TierRegistry, Detail: h.String(),
			Evidence: ev, TouchedDisk: disk}, trace
	}

	// 第 2 级：正在运行的进程。
	if len(procs) == 0 {
		trace = append(trace, "[2] 运行中进程：没有主程序在跑")
	} else {
		ordered := append(append([]ClassifiedProcess{}, inv.Mains...), inv.Processes...)
		for _, c := range ordered {
			if c.ExePath == "" {
				continue
			}
			trace = append(trace, fmt.Sprintf("[2] 取自进程 PID %d（%s）", c.PID, c.Kind))
			return ExeLocation{Path: c.ExePath, Tier: TierProcess,
				Detail:   fmt.Sprintf("PID %d，%s", c.PID, c.Kind),
				Evidence: fmt.Sprintf("PID %d 正在运行该路径（未访问磁盘）", c.PID)}, trace
		}
		trace = append(trace, "[2] 运行中进程都没有拿到 ExecutablePath")
	}

	// 第 3 级：默认路径。
	var defaults []string
	if p.Defaults != nil {
		defaults = p.Defaults()
	}
	for _, path := range defaults {
		ev, disk, ok := verify(path)
		if !ok {
			trace = append(trace, fmt.Sprintf("[3] 默认路径不存在：%q", path))
			continue
		}
		trace = append(trace, fmt.Sprintf("[3] 默认路径命中：%q", path))
		return ExeLocation{Path: path, Tier: TierDefault, Detail: "默认安装位置",
			Evidence: ev, TouchedDisk: disk}, trace
	}

	trace = append(trace, "三级查找全部落空")
	return ExeLocation{}, trace
}
