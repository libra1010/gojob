// Command hello 是 README「接入自己的应用」那段例子的可编译版本。
//
// 放在仓库里而不是只写在文档里，是因为发布脚本会 `go vet ./...` ——
// README 里的代码片段一旦与 SDK 的签名漂移，编译就会红，而不是把错的示例发给用户。
//
// 跑起来：先起 Server（go run ./cmd/gojob-server），再
//
//	go run ./examples/worker
//
// 然后在控制台建一个任务，处理器类名填 hello。
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"gojob/worker"
)

type hello struct{}

func (hello) Process(ctx *worker.TaskContext) error {
	select {
	case <-ctx.Done(): // 被服务端停止：协作式取消，处理器要自己看一眼 ctx
		return ctx.Err()
	default:
	}
	ctx.SetResult("hello") // 实例结果；MAP_REDUCE 下会进 reduce 的输入
	return nil
}

func main() {
	worker.Register("hello", hello{}) // 名字对应任务配置里的"处理器类名"
	w := worker.New(worker.Config{
		ServerAddress: "http://127.0.0.1:7800", // 多副本用 ServerAddresses，worker 自己容错
		AppId:         "demo_app",
		Password:      "demo123",
	})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Run 阻塞；ctx 取消时先 drain（等在跑的任务执行并回报完）再退出
	log.Fatal(w.Run(ctx))
}
