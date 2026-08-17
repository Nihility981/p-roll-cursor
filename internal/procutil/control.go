package procutil

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// 本文件实现关闭与启动 Cursor 的能力。
//
// ###########################################################################
// # 警告：截至目前，这些函数在本仓库里**没有任何调用点**，这是刻意的。       #
// # 开发这套工具时用的就是 Cursor 本身，一旦执行关闭，正在进行的会话会立刻   #
// # 断掉。probe 只会打印「将要执行什么」，绝不执行。                          #
// # 新增调用点前请确认不是在被关闭的那个 Cursor 里跑。                       #
// ###########################################################################
//
// 与原 Rust 实现的区别：原实现直接 `taskkill /PID <pid> /T /F` 整棵进程树强杀，
// 未保存的编辑会直接丢失。这里改成两段式——先发 WM_CLOSE 让 Cursor 自己走正常
// 退出流程（有机会保存），只有等到超时才升级为强杀。

// StopOptions 控制关闭时的等待策略。
type StopOptions struct {
	// GraceTimeout 是优雅关闭的最长等待时间，超时后升级为强杀。
	GraceTimeout time.Duration
	// ForceTimeout 是强杀后确认进程消失的最长等待时间。
	ForceTimeout time.Duration
	// PollInterval 是轮询进程是否退出的间隔。
	PollInterval time.Duration
}

// DefaultStopOptions 的轮询间隔沿用原 Rust 实现的 350ms；
// 优雅关闭给 10 秒，足够 Cursor 落盘，又不至于让用户干等太久。
func DefaultStopOptions() StopOptions {
	return StopOptions{
		GraceTimeout: 10 * time.Second,
		ForceTimeout: 5 * time.Second,
		PollInterval: 350 * time.Millisecond,
	}
}

// GracefulKillCommand 返回优雅关闭的命令。
// 不带 /F：taskkill 此时发的是 WM_CLOSE，等同于用户点窗口关闭按钮。
func GracefulKillCommand(pid int) []string {
	return []string{"taskkill", "/PID", strconv.Itoa(pid)}
}

// ForceKillCommand 返回强杀命令。/T 连同子进程整棵树，/F 强制终止。
// 未保存的编辑会丢，只应在优雅关闭超时后使用。
func ForceKillCommand(pid int) []string {
	return []string{"taskkill", "/PID", strconv.Itoa(pid), "/T", "/F"}
}

// StartCommand 返回启动 Cursor 的命令。
func StartCommand(exePath string, args ...string) []string {
	return append([]string{exePath}, args...)
}

// Runner 执行外部命令。抽成接口是为了让关闭流程可以用假实现做单元测试，
// 测试时绝不会真的调用 taskkill。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// ExecRunner 是真正会执行命令的实现。
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

// AliveFunc 判断进程是否仍然存在。
type AliveFunc func(ctx context.Context, pid int) (bool, error)

// StopReport 记录关闭过程实际发生了什么。
type StopReport struct {
	PID int
	// GracefulSent 表示优雅关闭命令已发出。
	GracefulSent bool
	// ExitedGracefully 表示进程在超时前自行退出，没有被强杀。
	ExitedGracefully bool
	// Escalated 表示优雅关闭超时，升级成了强杀。
	Escalated bool
	// Elapsed 是从发出第一条命令到确认退出的总耗时。
	Elapsed time.Duration
	// Commands 是实际执行过的命令，便于事后审计。
	Commands [][]string
}

// StopCursor 两段式关闭 Cursor：先优雅，超时才强杀。
//
// 调用前务必确认目标不是当前会话所在的 Cursor（见文件顶部警告）。
func StopCursor(ctx context.Context, pid int, opt StopOptions, r Runner, alive AliveFunc) (StopReport, error) {
	report := StopReport{PID: pid}
	start := time.Now()

	gone, err := alive(ctx, pid)
	if err != nil {
		return report, fmt.Errorf("检查进程 %d 状态失败: %w", pid, err)
	}
	if !gone {
		report.ExitedGracefully = true
		return report, nil
	}

	// 第一段：发 WM_CLOSE，让 Cursor 自己保存并退出。
	cmd := GracefulKillCommand(pid)
	report.Commands = append(report.Commands, cmd)
	report.GracefulSent = true
	// 这里刻意忽略返回码：进程可能在命令送达前就退出了，taskkill 会报错，
	// 但那恰恰是我们想要的结果，真正的判据是后面的存活轮询。
	_ = r.Run(ctx, cmd[0], cmd[1:]...)

	exited, err := waitGone(ctx, pid, opt.GraceTimeout, opt.PollInterval, alive)
	if err != nil {
		return report, err
	}
	if exited {
		report.ExitedGracefully = true
		report.Elapsed = time.Since(start)
		return report, nil
	}

	// 第二段：超时了，升级为强杀。到这一步未保存的编辑会丢。
	report.Escalated = true
	force := ForceKillCommand(pid)
	report.Commands = append(report.Commands, force)
	if err := r.Run(ctx, force[0], force[1:]...); err != nil {
		return report, fmt.Errorf("强制终止进程 %d 失败: %w", pid, err)
	}

	exited, err = waitGone(ctx, pid, opt.ForceTimeout, opt.PollInterval, alive)
	if err != nil {
		return report, err
	}
	report.Elapsed = time.Since(start)
	if !exited {
		return report, fmt.Errorf("进程 %d 在强制终止后仍然存在", pid)
	}
	return report, nil
}

// waitGone 轮询等待进程消失，返回是否在超时前退出。
func waitGone(ctx context.Context, pid int, timeout, interval time.Duration, alive AliveFunc) (bool, error) {
	if interval <= 0 {
		interval = 350 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		still, err := alive(ctx, pid)
		if err != nil {
			return false, fmt.Errorf("检查进程 %d 状态失败: %w", pid, err)
		}
		if !still {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// StartCursor 启动 Cursor 并返回新进程的 PID。
//
// 不等待退出，也不继承标准流：启动后 Cursor 应当独立于本进程存活。
// 同样从未被调用，见文件顶部警告。
func StartCursor(exePath string, args ...string) (int, error) {
	if exePath == "" {
		return 0, fmt.Errorf("未提供主程序路径")
	}
	cmd := exec.Command(exePath, args...)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动 %s 失败: %w", exePath, err)
	}
	pid := cmd.Process.Pid
	// 不 Wait 会留下僵尸子进程记录，起个 goroutine 收掉。
	go func() { _ = cmd.Wait() }()
	return pid, nil
}
