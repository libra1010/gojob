// gojob-demo：演示业务方如何内嵌 worker SDK —— 注册处理器 + 启动 + 优雅停止收单。
// curl -X POST localhost:7799/drain   停止接收新任务（运行中任务继续跑完）
// curl -X POST localhost:7799/resume  恢复接收
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gojob/worker"
)

type helloProcessor struct{}

func (helloProcessor) Process(ctx *worker.TaskContext) error {
	ctx.Infof("hello 开始执行，静态参数=%s 运行时参数=%s", ctx.JobParams, ctx.InstanceParams)
	log.Printf("[hello] run task=%d instance=%d attempts=%d params=%s instanceParams=%s",
		ctx.TaskID, ctx.InstanceID, ctx.Attempts, ctx.JobParams, ctx.InstanceParams)
	time.Sleep(200 * time.Millisecond) // 模拟业务耗时
	ctx.Infof("hello 执行完成，耗时 200ms")
	return nil
}

// slowProcessor 长任务，用于观察 drain 后"旧任务继续执行"。
type slowProcessor struct{}

func (slowProcessor) Process(ctx *worker.TaskContext) error {
	log.Printf("[slow] start task=%d", ctx.TaskID)
	select {
	case <-time.After(20 * time.Second):
		log.Printf("[slow] done task=%d", ctx.TaskID)
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// failOnce 首次执行失败、重试后成功，用于验证失败重试链路。
type failOnceProcessor struct {
	mu   sync.Mutex
	seen map[uint64]bool
}

func (p *failOnceProcessor) Process(ctx *worker.TaskContext) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx.Infof("failOnce 第 %d 次尝试，taskId=%d", ctx.Attempts, ctx.TaskID)
	if !p.seen[ctx.TaskID] {
		p.seen[ctx.TaskID] = true
		ctx.Warnf("模拟一次失败，等待调度重试")
		return fmt.Errorf("simulated failure on attempt %d", ctx.Attempts)
	}
	ctx.Infof("failOnce 第 %d 次尝试成功", ctx.Attempts)
	log.Printf("[failOnce] succeeded on attempt %d", ctx.Attempts)
	return nil
}

func envOverride(key string, target *string) {
	if v := os.Getenv(key); v != "" {
		*target = v
	}
}

func main() {
	server := flag.String("server", "http://127.0.0.1:7700", "gojob-server 地址；多副本用逗号分隔，worker 自带容错与 owner 跟随")
	appId := flag.String("app", "demo_app", "app id")
	password := flag.String("password", "demo123", "app password")
	drainAddr := flag.String("drain-addr", ":7799", "local admin endpoint for graceful offline")
	tag := flag.String("tag", "", "worker tag，供 SPECIFY 分发策略匹配（tagIn/tagEquals）")
	aesKey := flag.String("aes-key", "", "注册口令加密 key（64 位 hex），须与服务端 GOJOB_WORKER_AES_KEYS 之一一致；留空=口令明文送上来")
	flag.Parse()

	envOverride("GOJOB_WORKER_SERVER", server)
	envOverride("GOJOB_WORKER_APP", appId)
	envOverride("GOJOB_WORKER_PASSWORD", password)
	envOverride("GOJOB_WORKER_DRAIN_ADDR", drainAddr)
	envOverride("GOJOB_WORKER_TAG", tag)
	envOverride("GOJOB_WORKER_AES_KEY", aesKey)

	worker.Register("hello", helloProcessor{})
	worker.Register("slow", slowProcessor{})
	worker.Register("failOnce", &failOnceProcessor{seen: map[uint64]bool{}})
	registerMapProcessors() // broadcastEcho / mapSplit / mapFailOne / deepMap / mrSum

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	w := worker.New(worker.Config{
		ServerAddresses: strings.Split(*server, ","), // 单地址也走这里：normalizeServers 会去空去重
		AppId:           *appId,
		Password:        *password,
		Tag:             *tag,
		AesKey:          *aesKey,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /drain", func(rw http.ResponseWriter, r *http.Request) {
		w.StopReceivingNewTasks()
		fmt.Fprintf(rw, "receiving=%v\n", w.IsReceiving())
	})
	mux.HandleFunc("POST /resume", func(rw http.ResponseWriter, r *http.Request) {
		w.ResumeReceivingNewTasks()
		fmt.Fprintf(rw, "receiving=%v\n", w.IsReceiving())
	})
	go func() {
		log.Printf("demo drain endpoint on %s", *drainAddr)
		_ = http.ListenAndServe(*drainAddr, mux)
	}()

	if err := w.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
