//go:build !windows

package procutil

import (
	"context"
	"errors"
)

// 本包目前只实现 Windows。保留这些桩函数是为了让 go build ./... 在其他平台
// 上依然能过，便于跨平台做代码检查。
var errUnsupported = errors.New("procutil: 进程与路径发现目前只实现了 Windows")

func DefaultProviders(ctx context.Context) Providers {
	return Providers{
		Registry:  ReadRegistry,
		Processes: func() ([]ProcessRecord, error) { return ListProcesses(ctx) },
		Defaults:  DefaultPaths,
		Stat:      statExists,
	}
}

func ReadRegistry() ([]RegistryHit, error) { return nil, errUnsupported }

func ListProcesses(context.Context) ([]ProcessRecord, error) { return nil, errUnsupported }

func DefaultPaths() []string { return nil }

func statExists(string) bool { return false }
