// Package paths 负责解析本机 Cursor 的数据目录与关键文件路径。
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// CursorPaths 描述一套 Cursor 数据文件位置。
type CursorPaths struct {
	// AppDir 是 Cursor 的用户数据根目录，Windows 下为 %APPDATA%\Cursor
	AppDir string
	// GlobalStorage 是 <AppDir>/User/globalStorage
	GlobalStorage string
	// StateDB 是 state.vscdb 本体
	StateDB string
	// StateDBWAL / StateDBSHM 是 WAL 模式下的伴随文件
	StateDBWAL string
	StateDBSHM string
	// OptionsFile 是 state.vscdb.options.json，内容形如 {"useWAL": true}
	OptionsFile string
}

// Resolve 返回当前平台下的 Cursor 路径集合。
func Resolve() (*CursorPaths, error) {
	appDir, err := resolveAppDir()
	if err != nil {
		return nil, err
	}

	globalStorage := filepath.Join(appDir, "User", "globalStorage")
	stateDB := filepath.Join(globalStorage, "state.vscdb")

	return &CursorPaths{
		AppDir:        appDir,
		GlobalStorage: globalStorage,
		StateDB:       stateDB,
		StateDBWAL:    stateDB + "-wal",
		StateDBSHM:    stateDB + "-shm",
		OptionsFile:   stateDB + ".options.json",
	}, nil
}

func resolveAppDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("无法定位本机数据目录：环境变量 APPDATA 为空")
		}
		return filepath.Join(appData, "Cursor"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法定位用户主目录: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "Cursor"), nil
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "Cursor"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法定位用户主目录: %w", err)
		}
		return filepath.Join(home, ".config", "Cursor"), nil
	}
}

// FileInfo 是对单个文件存在性与大小的简单描述，便于 CLI 展示。
type FileInfo struct {
	Path   string
	Exists bool
	Size   int64
}

// Stat 只读地探测文件状态，任何错误都退化为「不存在」而不是中断流程。
func Stat(path string) FileInfo {
	info := FileInfo{Path: path}
	st, err := os.Stat(path)
	if err != nil {
		return info
	}
	info.Exists = true
	info.Size = st.Size()
	return info
}

// HumanSize 把字节数格式化成便于阅读的形式。
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
