//go:build windows

package procutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

// DefaultProviders 组装真机采集器。
func DefaultProviders(ctx context.Context) Providers {
	return Providers{
		Registry:  ReadRegistry,
		Processes: func() ([]ProcessRecord, error) { return ListProcesses(ctx) },
		Defaults:  DefaultPaths,
		Stat:      statExists,
	}
}

const uninstallPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`

// ReadRegistry 扫描 Uninstall 项找出 Cursor 的安装位置。
//
// HKLM 与 HKCU 都要查：系统级安装写在 HKLM，per-user 安装写在 HKCU。
// 64 位与 32 位（WOW6432Node）两个视图也都要覆盖，否则会漏。
//
// 这一步只读注册表字符串，不访问任何磁盘路径。
func ReadRegistry() ([]RegistryHit, error) {
	roots := []struct {
		name string
		key  registry.Key
	}{
		{"HKLM", registry.LOCAL_MACHINE},
		{"HKCU", registry.CURRENT_USER},
	}
	views := []struct {
		name string
		flag uint32
	}{
		{"64 位", registry.WOW64_64KEY},
		{"32 位 WOW6432Node", registry.WOW64_32KEY},
	}

	var hits []RegistryHit
	seen := map[string]bool{}
	var errs []string

	for _, root := range roots {
		for _, view := range views {
			base, err := registry.OpenKey(root.key, uninstallPath, registry.ENUMERATE_SUB_KEYS|view.flag)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s(%s): %v", root.name, view.name, err))
				continue
			}
			names, err := base.ReadSubKeyNames(-1)
			base.Close()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s(%s) 枚举子项: %v", root.name, view.name, err))
				continue
			}

			for _, name := range names {
				sub, err := registry.OpenKey(root.key, uninstallPath+`\`+name, registry.QUERY_VALUE|view.flag)
				if err != nil {
					continue
				}
				display, _, _ := sub.GetStringValue("DisplayName")
				install, _, _ := sub.GetStringValue("InstallLocation")
				version, _, _ := sub.GetStringValue("DisplayVersion")
				sub.Close()

				if !looksLikeCursor(name, display, install) {
					continue
				}
				// HKCU 下 64/32 视图指向同一物理位置，会重复命中，去掉重复项。
				key := root.name + "|" + name + "|" + install
				if seen[key] {
					continue
				}
				seen[key] = true

				hits = append(hits, RegistryHit{
					Root: root.name, View: view.name, KeyName: name,
					DisplayName: display, DisplayVersion: version, InstallLocation: install,
				})
			}
		}
	}

	if len(hits) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("注册表读取失败：%s", strings.Join(errs, "; "))
	}
	return hits, nil
}

// looksLikeCursor 判断一条 Uninstall 记录是不是 Cursor。
// 只匹配 DisplayName / 键名，不去 stat InstallLocation。
func looksLikeCursor(keyName, displayName, installLocation string) bool {
	for _, s := range []string{displayName, keyName} {
		if strings.Contains(strings.ToLower(s), "cursor") {
			return true
		}
	}
	// 键名是 GUID 时 DisplayName 可能缺失，退而看安装目录名。
	base := strings.ToLower(filepath.Base(strings.TrimRight(installLocation, `\/`)))
	return base == "cursor"
}

// wmiProcessQuery 用 WMI 取进程信息。
//
// 选 WMI 而不是 CreateToolhelp32Snapshot，是因为判定主进程必须要有完整命令行
// （要看 --type=），快照 API 拿不到。ExecutablePath 也由 WMI 直接给出字符串，
// **全程不需要访问 exe 所在磁盘**——本机 Cursor 装在 D 盘（坏盘），这一点很关键。
const wmiProcessQuery = `$ErrorActionPreference='Stop'
[Console]::OutputEncoding=[Text.Encoding]::UTF8
$p = Get-CimInstance Win32_Process -Filter "Name='Cursor.exe'" |
     Select-Object ProcessId,ParentProcessId,ExecutablePath,CommandLine
ConvertTo-Json -InputObject @($p) -Depth 3 -Compress`

// ListProcesses 返回当前所有 Cursor.exe 进程。这是只读查询。
func ListProcesses(ctx context.Context) ([]ProcessRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-Command", wmiProcessQuery)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("查询 Win32_Process 失败: %w", err)
	}

	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil, nil
	}

	var rows []struct {
		ProcessID       int    `json:"ProcessId"`
		ParentProcessID int    `json:"ParentProcessId"`
		ExecutablePath  string `json:"ExecutablePath"`
		CommandLine     string `json:"CommandLine"`
	}
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return nil, fmt.Errorf("解析 Win32_Process 输出失败: %w", err)
	}

	records := make([]ProcessRecord, 0, len(rows))
	for _, r := range rows {
		records = append(records, ProcessRecord{
			PID: r.ProcessID, ParentPID: r.ParentProcessID,
			ExePath: r.ExecutablePath, CommandLine: r.CommandLine,
		})
	}
	return records, nil
}

// DefaultPaths 是第 3 级兜底，系统级与用户级两种默认位置都要覆盖。
//
// 实地核对推翻过一个假设：Cursor **不一定**在 %LOCALAPPDATA%\Programs\Cursor，
// 本机就是系统级安装。所以两种都列，且这一级永远排在注册表与进程之后。
func DefaultPaths() []string {
	var out []string
	add := func(base string, parts ...string) {
		if strings.TrimSpace(base) == "" {
			return
		}
		out = append(out, filepath.Join(append([]string{base}, parts...)...))
	}
	add(os.Getenv("LOCALAPPDATA"), "Programs", "Cursor", ExeName)
	add(os.Getenv("ProgramFiles"), "cursor", ExeName)
	add(os.Getenv("ProgramFiles(x86)"), "cursor", ExeName)
	return out
}

// statExists 是存在性验证的**最后手段**。
//
// 它是本包唯一真正接触磁盘的地方。Cursor 装在 D 盘而 D/E/F 都是坏盘，
// 对坏盘 stat 有卡死风险，所以 Locate 会优先用「进程正在运行该路径」当证据，
// 只有在没有任何进程佐证时才落到这里。
func statExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
