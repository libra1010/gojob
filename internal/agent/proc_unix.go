//go:build linux || darwin

package agent

import (
	"os/exec"
	"syscall"
)

// launchInNewGroup 让脚本自成进程组，取消时把整组一起带走。
//
// 为什么不用 exec.CommandContext 的默认行为：它只 kill 直接子进程 /bin/sh，
// 脚本里 nohup/& 起来的孙进程会变成孤儿继续跑，节点上就会出现"任务已经 STOPPED
// 但进程还在"的状况；而调度节点上无人值守的残留进程，比一次执行失败严重得多。
func launchInNewGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// 负 pid = 整个进程组；进程已经退了就是 ESRCH，忽略即可（错误由 ctx 侧表达）
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil
	}
	return nil
}
