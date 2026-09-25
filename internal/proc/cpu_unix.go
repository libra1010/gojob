//go:build unix

package proc

import (
	"syscall"
	"time"
)

// cpuTime 用 getrusage：Linux 与 macOS 都能拿到本进程累计 CPU 时间，无需按平台分支。
func cpuTime() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(int64(ru.Utime.Usec))*time.Microsecond
	sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(int64(ru.Stime.Usec))*time.Microsecond
	return user + sys, true
}
