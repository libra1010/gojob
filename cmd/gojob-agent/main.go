// gojob-agent：零代码的通用执行节点。不写 Go 代码，配好项目口令与脚本库就能领任务跑脚本。
//
// 这个进程不监听任何端口。要让它停止收单：Ctrl-C 走优雅下线（在跑的任务执行完并回报），
// 或者在控制台「节点 → 停止接单」上改那一台节点的库字段（服务端不再把新任务派给它）。
//
//	GOJOB_WORKER_SERVER=http://127.0.0.1:7700 GOJOB_WORKER_APP=demo_app \
//	GOJOB_WORKER_PASSWORD=demo123 GOJOB_AGENT_SCRIPT_DIR=/srv/gojob/scripts \
//	gojob-agent
//
// 控制台的「处理器信息」填 tech.powerjob.official.processors.impl.script.ShellProcessor
// （或短名 shell），脚本正文放「任务参数」——与兼容协议的官方脚本处理器同一种配法。
//
// 安全默认值刻意偏保守：只执行 GOJOB_AGENT_SCRIPT_DIR 里预置的脚本。原因见 README
// 「gojob-agent：不写代码的执行节点」一节——建任务只要项目口令，若参数即脚本，
// 拿到口令就等于拿到本机的命令执行权。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gojob/internal/agent"
	"gojob/internal/protocol"
	"gojob/worker"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("环境变量 %s=%q 不是合法布尔值，按 %v 处理", key, v, def)
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("环境变量 %s=%q 不是合法正整数，按 %d 处理", key, v, def)
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("环境变量 %s=%q 不是合法时长（如 30s/5m），按 %s 处理", key, v, def)
		return def
	}
	return d
}

func main() {
	log.SetPrefix("[gojob-agent] ")
	server := flag.String("server", envOr("GOJOB_WORKER_SERVER", "http://127.0.0.1:7700"),
		"gojob-server 地址；多副本用逗号分隔，worker 自带容错与 owner 跟随")
	appId := flag.String("app", envOr("GOJOB_WORKER_APP", "demo_app"), "项目 ID")
	password := flag.String("password", envOr("GOJOB_WORKER_PASSWORD", ""),
		"项目口令（建议用环境变量 GOJOB_WORKER_PASSWORD 传，别写进命令行）")
	tag := flag.String("tag", envOr("GOJOB_WORKER_TAG", "executor"),
		"机器标签：默认 executor，便于用 SPECIFY 分发策略把脚本任务只派给执行节点")
	concurrency := flag.Int("concurrency", envInt("GOJOB_AGENT_MAX_CONCURRENCY", 8),
		"同时执行的脚本数上限")
	scripts := flag.String("script-dir", envOr("GOJOB_AGENT_SCRIPT_DIR", ""),
		"脚本库目录，只允许执行其中预置的脚本；留空表示不启用库模式")
	inline := flag.Bool("allow-inline-script", envBool("GOJOB_AGENT_ALLOW_INLINE_SCRIPTS", false),
		"允许把任务参数当脚本文本执行（含 http/https 下载）；这等于把本机命令执行权交给项目口令持有者")
	workspace := flag.String("script-workspace", envOr("GOJOB_AGENT_WORKSPACE", ""),
		"内联脚本的工作目录，默认 {user.home}/gojob/agent")
	timeout := flag.Duration("script-timeout", envDuration("GOJOB_AGENT_SCRIPT_TIMEOUT", 0),
		"单次脚本执行上限，0=不限（仍可被控制台的停止指令打断）")
	flag.Parse()

	if *password == "" {
		log.Fatal("缺少项目口令：--password 或 GOJOB_WORKER_PASSWORD")
	}
	if *scripts != "" {
		if st, err := os.Stat(*scripts); err != nil || !st.IsDir() {
			log.Fatalf("脚本库目录不可用：%s（%v）", *scripts, err)
		}
	} else if !*inline {
		log.Fatal("既没配 GOJOB_AGENT_SCRIPT_DIR 也没开 GOJOB_AGENT_ALLOW_INLINE_SCRIPTS，这个 agent 什么都执行不了")
	}

	sh := &agent.Script{
		ScriptDir:   *scripts,
		AllowInline: *inline,
		Workspace:   *workspace,
		Timeout:     *timeout,
	}
	// 主名用兼容协议官方处理器的类全名（同一份任务配置两边通用），短名 shell 是 gojob 别名
	worker.Register(protocol.OfficialShellProcessor, sh)
	worker.Register(agent.ShellAliasName, sh)
	if *inline {
		log.Printf("警告：已允许执行任务参数里的脚本文本/下载链接——任何能建任务（拿到项目口令）的人" +
			"都能在本机执行命令，请只在隔离网段这样部署")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	w := worker.New(worker.Config{
		ServerAddresses: strings.Split(*server, ","), // 单地址也走这里：normalizeServers 会去空去重
		AppId:           *appId,
		Password:        *password,
		Tag:             *tag,
		MaxConcurrency:  *concurrency,
		// 注册口令加密只走环境变量：key 与口令同为凭据，不该出现在 --aes-key 这种 ps 可见的地方
		AesKey: envOr("GOJOB_WORKER_AES_KEY", ""),
	})
	log.Printf("执行节点启动：app=%s 脚本库=%s 允许内联脚本=%t 单次上限=%s 并发=%d tag=%s",
		*appId, orNA(*scripts), *inline, orUnlimited(*timeout), *concurrency, *tag)

	if err := w.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func orNA(s string) string {
	if s == "" {
		return "未配置"
	}
	return s
}

func orUnlimited(d time.Duration) string {
	if d <= 0 {
		return "不限"
	}
	return d.String()
}
