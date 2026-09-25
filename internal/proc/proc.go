// Package proc 读取"本进程自身"的操作系统级指标，server 节点自监控与 worker SDK 共用。
//
// 取不到的项一律返回 0/false 并由调用方显示成"—"，绝不猜数：
// RSS 与 fd 数走 /proc，只有 Linux（含容器）有；CPU 时间走 getrusage，unix 全系可用。
package proc

import "time"

// CPUTime 本进程累计消耗的 CPU 时间（用户+内核），false=本平台取不到。
func CPUTime() (time.Duration, bool) { return cpuTime() }

// RSSMb 进程常驻物理内存（MB），0=取不到。
func RSSMb() float64 { return rssMb() }

// OpenFDs 进程打开的文件描述符数（含 socket），0=取不到。
func OpenFDs() int { return openFDs() }
