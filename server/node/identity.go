package node

import (
	"fmt"
	"os"
	"time"
)

// NodeID 本进程的稳定身份：hostname-pid-启动纳秒。
// 进程重启会换 ID，所以多副本列表里的"旧副本"不会被误当成还活着（判活只看心跳时间）。
func NodeID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), time.Now().UnixNano())
}

// Hostname 只取机器名，供页面分列展示。
func Hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		return "unknown"
	}
	return h
}
