//go:build !(linux || darwin)

package agent

import "os/exec"

// launchInNewGroup 在非 unix 平台上没有进程组可设，退化为 exec.CommandContext 的默认行为
// （只 kill 直接子进程）。gojob-agent 的目标部署环境是 linux 容器/主机。
func launchInNewGroup(cmd *exec.Cmd) error { return nil }
