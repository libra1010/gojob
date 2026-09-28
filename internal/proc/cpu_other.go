//go:build !unix

package proc

import "time"

// cpuTime 非 unix（Windows）没有 getrusage，调用方按"取不到"处理。
func cpuTime() (time.Duration, bool) { return 0, false }
