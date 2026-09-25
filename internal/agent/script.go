// Package agent 是 gojob-agent（零代码通用执行节点）所用的官方处理器集合。
//
// 这些都是普通的 worker.Processor 实现：业务方也可以只 import 其中某个处理器，
// 或按自己的方式组装 main；gojob-agent 只是"全都注册好、参数从环境读"的那一层。
package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gojob/worker"
)

// ShellAliasName 是官方 Shell 处理器的 gojob 短名别名；主名用 PowerJob 的类全名
// （protocol.OfficialShellProcessor），这样同一份任务配置在两边通用。
const ShellAliasName = "shell"

// scriptDirName 与 PowerJob AbstractScriptProcessor 的工作子目录同名，便于两边对照。
const scriptDirName = "official_script_processor"

// downloadPrefixes 照搬 PowerJob 的 DOWNLOAD_PROTOCOL：参数以这些前缀开头即视为下载链接。
var downloadPrefixes = []string{"http://", "https://", "ftp://"}

// Script 执行 Shell 脚本，行为对齐 PowerJob 的
// tech.powerjob.official.processors.impl.script.ShellProcessor：脚本内容取执行参数
// （instanceParams 非空优先，否则 jobParams），落成脚本文件、chmod 755、用 /bin/sh 执行，
// 标准输出与错误输出逐行同步到任务日志通道。
//
// 在 PowerJob 之上多了一道必须说清的闸：默认只执行 ScriptDir 里预置好的脚本，想让"任务参数
// 里带来的脚本文本/下载链接"也能跑，必须显式 AllowInline。理由是权限边界——建任务只要项目
// 口令，若参数即脚本，拿到口令就等于拿到 agent 所在主机的命令执行权。
type Script struct {
	// ScriptDir 脚本库目录；空表示不启用库模式。
	ScriptDir string
	// AllowInline 允许执行任务参数里的脚本文本/下载链接。
	AllowInline bool
	// Workspace 内联脚本的工作目录，默认 {user.home}/gojob/agent
	// （PowerJob 用 {user.home}/powerjob/worker）。
	Workspace string
	// Timeout 单次执行上限，0 = 不限制（对齐 PowerJob instanceTimeLimit=0 的语义）。
	Timeout time.Duration
}

func (s *Script) Process(ctx *worker.TaskContext) error {
	// PowerJob 的 CommonUtils.parseParams：实例参数优先，其次任务参数
	params := strings.TrimSpace(ctx.InstanceParams)
	if params == "" {
		params = strings.TrimSpace(ctx.JobParams)
	}
	if params == "" {
		return errors.New("脚本内容为空：请在任务参数（或实例参数）里填脚本库中的脚本名，或脚本正文")
	}
	if runtime.GOOS == "windows" {
		// PowerJob 同样的限制：Windows 上 shell 脚本无法执行（要用 CMD/PowerShell 处理器）
		return fmt.Errorf("当前 OS 为 %s，shell 脚本无法执行", runtime.GOOS)
	}
	scriptPath, args, err := s.resolve(params, ctx.InstanceID)
	if err != nil {
		return err
	}
	ctx.Infof("[SYSTEM] 脚本处理器开始执行，可执行文件=%s 参数=%v 脚本库=%s 允许内联脚本=%t",
		scriptPath, args, orNA(s.ScriptDir), s.AllowInline)
	return s.run(ctx, scriptPath, args)
}

// resolve 决定这次执行哪个文件：脚本库里预置的脚本，还是内联落到工作目录的脚本。
func (s *Script) resolve(params string, instanceID uint64) (string, []string, error) {
	fields := strings.Fields(params)
	name, args := fields[0], fields[1:]
	if p, ok := s.fromLibrary(name); ok {
		return p, args, nil
	}
	if !s.AllowInline {
		return "", nil, fmt.Errorf("脚本库中找不到 %q（脚本库=%s）；要执行任务参数里直接给的脚本文本，需显式设 GOJOB_AGENT_ALLOW_INLINE_SCRIPTS=true",
			name, orNA(s.ScriptDir))
	}
	// 内联模式没有"参数"这一说：整段参数都是脚本内容，与 PowerJob 一致
	p, err := s.materialize(strings.TrimSpace(params), instanceID)
	return p, nil, err
}

// fromLibrary 只接受"脚本库目录下的一个普通文件名"：不含路径分隔符、且拼接后仍落在库目录内，
// 于是 ../、绝对路径等一切越界写法都到不了 exec。
func (s *Script) fromLibrary(name string) (string, bool) {
	if s.ScriptDir == "" || name == "" || strings.ContainsAny(name, `/\`) || name == ".." {
		return "", false
	}
	dir, err := filepath.Abs(s.ScriptDir)
	if err != nil {
		return "", false
	}
	p := filepath.Join(dir, name)
	if filepath.Dir(p) != dir {
		return "", false
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return p, true
}

// materialize 把脚本文本写成可执行文件，路径与命名沿用 PowerJob
// （{workspace}/official_script_processor/shell_{instanceId}.sh）。
//
// 与 PowerJob 有一处有意差异：它遇到同名文件直接复用（prepareScriptFile 里 script.exists()
// 即返回），改过的脚本会静默按旧内容再跑一遍；这里每次覆盖写入。
func (s *Script) materialize(script string, instanceID uint64) (string, error) {
	dir := filepath.Join(s.workspace(), scriptDirName)
	path := filepath.Join(dir, fmt.Sprintf("shell_%d.sh", instanceID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建脚本工作目录 %s: %w", dir, err)
	}
	for _, prefix := range downloadPrefixes {
		if strings.HasPrefix(script, prefix) {
			return path, download(script, path)
		}
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return "", fmt.Errorf("写入脚本文件 %s: %w", path, err)
	}
	// WriteFile 的权限位会被"文件已存在"挡住（O_TRUNC 不改 mode），显式再 chmod 一次
	if err := os.Chmod(path, 0o755); err != nil {
		return "", fmt.Errorf("chmod %s: %w", path, err)
	}
	return path, nil
}

// download 对应 PowerJob 的 FileUtils.copyURLToFile(url, file, 5000, 300000)。
// ftp 在 Java 里由 URL 直接支持，Go 的 http.Client 做不到，明确拒绝而不是悄悄跑错。
func download(url, path string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("不支持的下载地址协议：%s（只支持 http/https）", firstN(url, 32))
	}
	hc := &http.Client{Timeout: 300 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("下载地址 %s: %w", firstN(url, 128), err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("下载脚本 %s: %w", firstN(url, 128), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("下载脚本 %s: HTTP %d", firstN(url, 128), resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	// 上限 32MB：脚本不该更大，也防止一个恶意链接把节点磁盘写满
	n, err := io.Copy(f, io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("下载脚本 %s: %w", firstN(url, 128), err)
	}
	if n == 0 {
		return fmt.Errorf("下载脚本 %s: 内容为空", firstN(url, 128))
	}
	return nil
}

// run 执行脚本并把输出逐行同步到任务日志。解释器固定 /bin/sh（PowerJob 的 SH_SHELL），
// 脚本自己写 shebang 不影响这一点——被 exec 的是文件路径，走的仍是 /bin/sh。
func (s *Script) run(ctx *worker.TaskContext, scriptPath string, args []string) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.Timeout > 0 {
		tctx, cancelTimeout := context.WithTimeout(runCtx, s.Timeout)
		defer cancelTimeout()
		runCtx = tctx
	}

	cmd := exec.CommandContext(runCtx, "/bin/sh", append([]string{scriptPath}, args...)...)
	cmd.Dir = filepath.Dir(scriptPath)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("GOJOB_TASK_ID=%d", ctx.TaskID),
		fmt.Sprintf("GOJOB_JOB_ID=%d", ctx.JobID),
		fmt.Sprintf("GOJOB_INSTANCE_ID=%d", ctx.InstanceID),
	)
	// 独立进程组 + 取消时整组带走：否则脚本自己起的后台进程会变成孤儿继续跑。
	if err := launchInNewGroup(cmd); err != nil {
		return fmt.Errorf("启动脚本 %s: %w", scriptPath, err)
	}
	// 孙进程赖着不走时也不能把 worker 的执行槽永久占住
	cmd.WaitDelay = 10 * time.Second

	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errOut, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动脚本 %s: %w", scriptPath, err)
	}
	stdout := streamTo(ctx, out, false)
	stderr := streamTo(ctx, errOut, true)
	waitErr := cmd.Wait()
	stdout.wait()
	stderr.wait()
	elapsed := time.Since(started).Truncate(time.Millisecond)

	if runCtx.Err() != nil {
		// 服务端停止指令（ctx 取消）或实例时间超限：都按取消收尾，停止路径会回报成 STOPPED
		return fmt.Errorf("脚本执行被取消（已运行 %s）：%w", elapsed, runCtx.Err())
	}
	if waitErr != nil {
		return fmt.Errorf("脚本退出码 %d，耗时 %s；[ERROR]: %s；[INPUT]: %s",
			exitCode(waitErr), elapsed, stderr.tail(), stdout.tail())
	}
	ctx.Infof("[SYSTEM] 脚本执行成功，耗时 %s，输出 %d 行", elapsed, stdout.lines())
	return nil
}

func (s *Script) workspace() string {
	if s.Workspace != "" {
		return s.Workspace
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "gojob-agent")
	}
	return filepath.Join(home, "gojob", "agent")
}

// tailBytes 是「执行结果」列里保留的输出尾部长度：整段输出已在任务日志通道里，
// 结果列再塞几十 MB 只会拖垮列表页。
const tailBytes = 2048

// copier 把子进程输出逐行同步到任务日志，并留最后若干行做结果摘要。
type copier struct {
	ch      chan struct{}
	linesN  int
	size    int
	kept    []string
	dropped bool
}

func streamTo(ctx *worker.TaskContext, r io.Reader, isErr bool) *copier {
	c := &copier{ch: make(chan struct{})}
	go func() {
		defer close(c.ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			line := sc.Text()
			c.linesN++
			c.keep(line)
			if isErr {
				ctx.Warnf("%s", line)
			} else {
				ctx.Infof("%s", line)
			}
		}
	}()
	return c
}

func (c *copier) keep(line string) {
	c.size += len(line) + 1
	c.kept = append(c.kept, line)
	for c.size > tailBytes && len(c.kept) > 1 {
		c.size -= len(c.kept[0]) + 1
		c.kept = c.kept[1:]
		c.dropped = true
	}
}

func (c *copier) wait() { <-c.ch }

func (c *copier) lines() int { return c.linesN }

func (c *copier) tail() string {
	if c.linesN == 0 {
		return "（无输出）"
	}
	s := strings.Join(c.kept, "\n")
	if c.dropped {
		return "...(前文已截断，完整输出见实例日志)...\n" + s
	}
	return s
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func orNA(s string) string {
	if s == "" {
		return "未配置"
	}
	return s
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ worker.Processor = (*Script)(nil)
