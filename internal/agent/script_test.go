package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gojob/worker"
)

// markEnv 是测试脚本用来报告"我确实跑了、参数是什么"的通道：处理器的日志管道在单测里不存在，
// 而子进程的输出会随任务日志走，落到文件里断言更直接。
const markEnv = "GOJOB_AGENT_TEST_MARK"

func taskCtx(instanceID uint64, jobParams, instanceParams string) *worker.TaskContext {
	return &worker.TaskContext{
		Context:        context.Background(),
		TaskID:         instanceID,
		JobID:          1,
		InstanceID:     instanceID,
		JobParams:      jobParams,
		InstanceParams: instanceParams,
	}
}

func markPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mark")
	t.Setenv(markEnv, p)
	return p
}

func readMark(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("脚本没有写 %s: %v", p, err)
	}
	return strings.TrimSpace(string(b))
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryScriptRunsAndPassesArgs(t *testing.T) {
	dir := t.TempDir()
	mark := markPath(t)
	writeFile(t, filepath.Join(dir, "hello.sh"), fmt.Sprintf("#!/bin/sh\necho \"$1\" > $%s\n", markEnv), 0o755)

	s := &Script{ScriptDir: dir, Workspace: t.TempDir()}
	if err := s.Process(taskCtx(1, "hello.sh world", "")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := readMark(t, mark); got != "world" {
		t.Fatalf("脚本参数没传进去，mark=%q", got)
	}
	// 库模式不该往工作目录里复制脚本
	if _, err := os.Stat(filepath.Join(s.Workspace, scriptDirName, "shell_1.sh")); err == nil {
		t.Fatal("库模式不该生成内联脚本文件")
	}
}

func TestInlineScriptDeniedByDefault(t *testing.T) {
	mark := markPath(t)
	s := &Script{Workspace: t.TempDir()}
	err := s.Process(taskCtx(2, "echo pwned > $"+markEnv, ""))
	if err == nil {
		t.Fatal("默认必须拒绝执行任务参数里直接给的脚本文本")
	}
	if !strings.Contains(err.Error(), "GOJOB_AGENT_ALLOW_INLINE_SCRIPTS") {
		t.Fatalf("错误信息要指明开哪个开关，实际：%v", err)
	}
	if _, e := os.Stat(mark); e == nil {
		t.Fatal("被拒绝的脚本不该执行")
	}
}

func TestInlineScriptRunsWhenAllowed(t *testing.T) {
	mark := markPath(t)
	ws := t.TempDir()
	s := &Script{Workspace: ws, AllowInline: true}
	script := fmt.Sprintf("#!/bin/sh\necho inline > $%s\n", markEnv)
	if err := s.Process(taskCtx(3, script, "")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := readMark(t, mark); got != "inline" {
		t.Fatalf("mark=%q", got)
	}
	// 路径与命名沿用 PowerJob：{workspace}/official_script_processor/shell_{instanceId}.sh
	if _, err := os.Stat(filepath.Join(ws, scriptDirName, "shell_3.sh")); err != nil {
		t.Fatalf("脚本文件位置不对：%v", err)
	}
}

// PowerJob 的 prepareScriptFile 遇到同名文件直接复用，改过的脚本会静默按旧内容再跑；
// 这里有意每次覆盖写入，这个用例钉住这个差异。
func TestInlineScriptIsRewrittenNotReused(t *testing.T) {
	mark := markPath(t)
	s := &Script{Workspace: t.TempDir(), AllowInline: true}
	ctx := taskCtx(4, fmt.Sprintf("echo first > $%s", markEnv), "")
	if err := s.Process(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readMark(t, mark); got != "first" {
		t.Fatalf("第一次 mark=%q", got)
	}
	ctx2 := taskCtx(4, fmt.Sprintf("echo second > $%s", markEnv), "")
	if err := s.Process(ctx2); err != nil {
		t.Fatal(err)
	}
	if got := readMark(t, mark); got != "second" {
		t.Fatalf("同一实例改脚本后应执行新内容，mark=%q", got)
	}
}

func TestInstanceParamsWinOverJobParams(t *testing.T) {
	mark := markPath(t)
	s := &Script{Workspace: t.TempDir(), AllowInline: true}
	err := s.Process(taskCtx(5,
		fmt.Sprintf("echo job > $%s", markEnv),
		fmt.Sprintf("echo instance > $%s", markEnv)))
	if err != nil {
		t.Fatal(err)
	}
	if got := readMark(t, mark); got != "instance" {
		t.Fatalf("parseParams 应让实例参数优先，mark=%q", got)
	}
}

// 脚本库只认"库目录下的一个文件名"：带路径分隔符、绝对路径、.. 一律不许命中库，
// 否则一个脚本名就能跳出目录。
func TestLibraryRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.Mkdir(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	mark := markPath(t)
	writeFile(t, filepath.Join(dir, "evil.sh"), fmt.Sprintf("echo evil > $%s\n", markEnv), 0o755)
	s := &Script{ScriptDir: lib, Workspace: t.TempDir()}
	for _, params := range []string{"../evil.sh", filepath.Join(dir, "evil.sh"), "sub/../evil.sh"} {
		err := s.Process(taskCtx(6, params, ""))
		if err == nil || !strings.Contains(err.Error(), "GOJOB_AGENT_ALLOW_INLINE_SCRIPTS") {
			t.Fatalf("%q 既不在库里，就该走\"内联未开\"的拒绝路径，实际 err=%v", params, err)
		}
		if _, e := os.Stat(mark); e == nil {
			t.Fatalf("%q 越界执行了库外脚本", params)
		}
	}
}

func TestNonZeroExitCodeFailsWithOutputTail(t *testing.T) {
	markPath(t)
	s := &Script{Workspace: t.TempDir(), AllowInline: true}
	err := s.Process(taskCtx(7, "echo to-stdout; echo to-stderr >&2; exit 3", ""))
	if err == nil {
		t.Fatal("非 0 退出码必须判失败")
	}
	for _, want := range []string{"退出码 3", "to-stderr", "to-stdout"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息缺少 %q：%v", want, err)
		}
	}
}

func TestEmptyParams(t *testing.T) {
	s := &Script{Workspace: t.TempDir(), AllowInline: true}
	if err := s.Process(taskCtx(8, "   ", "")); err == nil {
		t.Fatal("参数为空应报错")
	}
}

func TestUnsupportedDownloadProtocol(t *testing.T) {
	markPath(t)
	s := &Script{Workspace: t.TempDir(), AllowInline: true}
	err := s.Process(taskCtx(9, "ftp://example.invalid/a.sh", ""))
	if err == nil || !strings.Contains(err.Error(), "只支持 http/https") {
		t.Fatalf("ftp 下载应明确拒绝，实际 err=%v", err)
	}
}

// 停止/超时要连脚本自己起的子进程一起收，否则节点上会出现"实例已 STOPPED 但进程还在跑"。
func TestTimeoutKillsWholeProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell 脚本在 Windows 上本就不执行")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	markPath(t)
	s := &Script{Workspace: t.TempDir(), AllowInline: true, Timeout: 300 * time.Millisecond}
	started := time.Now()
	err := s.Process(taskCtx(10, fmt.Sprintf("sleep 30 & echo $! > %s; wait", pidFile), ""))
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("超时没有生效，耗时 %s", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "取消") {
		t.Fatalf("应报取消，实际 err=%v", err)
	}
	b, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("读不到 sleep 的 pid：%v", readErr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // 进程组已经没了
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("孙进程 %d 在取消后仍在运行（进程组没被带走）", pid)
}
