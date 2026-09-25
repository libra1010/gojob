//go:build linux

package proc

import (
	"os"
	"strconv"
	"strings"
)

var pageSize = float64(os.Getpagesize())

// rssMb 读 /proc/self/statm 第二列（常驻页数）× 页大小。
func rssMb() float64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseFloat(f[1], 64)
	if err != nil {
		return 0
	}
	return pages * pageSize / 1024 / 1024
}

// openFDs 数 /proc/self/fd 下的条目；容器里就是本进程的 fd 上限压力真实值。
func openFDs() int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(ents)
}
