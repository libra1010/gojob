//go:build !linux

package proc

// rssMb / openFDs 依赖 /proc，非 Linux（macOS 本地开发、Windows）取不到，
// 统一报 0，控制台对应列显示"—"，与 worker SDK 既有的退化口径一致。
func rssMb() float64 { return 0 }
func openFDs() int   { return 0 }
