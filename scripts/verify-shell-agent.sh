#!/usr/bin/env bash
# gojob-agent（零代码执行节点）实机验收：自己起 1 个 server + 1 个 agent + 1 个普通 demo worker，
# 逐项验证"脚本任务"这条链真的通、而且安全默认值真的在承重：
#   ① 脚本库里的预置脚本能执行，短名 shell 与兼容协议官方类全名等价，参数能传进脚本；
#   ② 默认拒绝"任务参数即脚本文本"，错误信息直接点名 GOJOB_AGENT_ALLOW_INLINE_SCRIPTS；
#   ③ 脚本 stdout/stderr 逐行进任务日志；非 0 退出码判失败并在结果里带 stderr 尾巴；
#   ④ SPECIFY + tagEquals:executor 把脚本任务只派给 agent（普通 worker 一次都收不到）；
#   ⑤ processorType=EXTERNAL 被 worker 明确拒绝，而不是含糊的"找不到处理器"；
#   ⑥ 停止实例会把脚本连同它自己起的后台子进程一起收走（不留孤儿）；
#   ⑦ 显式开 GOJOB_AGENT_ALLOW_INLINE_SCRIPTS 后，同一条内联任务立刻能跑；
#   ⑧ 老式兼容协议的 SHELL 任务导入后被改写成 BUILT_IN + 官方脚本处理器并真的跑通；
#   ⑨ PYTHON / EXTERNAL 的导入逐条给出可操作原因（不留静默哑弹）。
#
# 口令与 worker 票那条边界由 scripts/verify-worker-token.sh 负责，这里不重复。
#
# 用法: scripts/verify-shell-agent.sh      （可用 GO/PORT/APP/PW/SECRET 覆盖）
# 注意 JSON 一律先 printf 拼进变量再交给 curl：macOS 自带 bash 3.2 在双引号里嵌套 $()
# 再套转义引号时解析会错乱，别在这里踩。
set -u
GO=${GO:-go}
PORT=${PORT:-7881}
BASE=http://127.0.0.1:$PORT
APP=${APP:-shell_app}
PW=${PW:-shell123}
SECRET=${SECRET:-shell-agent-secret}
WORK=$(mktemp -d)
DB=$WORK/shell.db
LIB=$WORK/scripts
SBIN=$WORK/gojob-server
ABIN=$WORK/gojob-agent
WBIN=$WORK/gojob-demo
AGENT_LOG=$WORK/agent.log
WORKER_LOG=$WORK/worker.log
AGENT_PID_FILE=$WORK/agent.pid
pass=0
fail=0
TOKEN=

ok() { pass=$((pass + 1)); echo "  PASS  $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL  $1"; }
# 后台进程一律用 ( … & ) 起：不挂到本 shell 的作业控制上，收尾时才不会往输出里插
# 一堆 "Killed: 9" 的噪声（验收脚本的结论行得干净）。
cleanup() {
  for f in "$WORK"/*.pid; do
    [ -f "$f" ] && kill -9 "$(cat "$f")" 2>/dev/null
  done
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

jget() { # jget <json> <python 表达式，root 是已解析对象>
  printf '%s' "$1" | python3 -c "
import sys,json
try: root=json.load(sys.stdin)
except Exception as e: print('parse-error',e); sys.exit()
try: print($2)
except Exception as e: print('missing',e)
"
}
api() { local base=$1 path=$2; shift 2; curl -s -m 10 -X POST "$base$path" -H "X-GoJob-Token: $TOKEN" "$@"; }
api_get() { curl -s -m 10 "$1$2" -H "X-GoJob-Token: $TOKEN"; }
login() {
  local body='{"username":"admin","password":"admin123"}'
  TOKEN=$(curl -s -m 8 -X POST "$1/openapi/user/login" -H 'Content-Type: application/json' -d "$body" \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  [ -n "$TOKEN" ] && ok "$1 登录换到会话票" || { bad "$1 登录失败"; exit 1; }
}
wait_health() {
  local i=0
  until curl -sf -m 2 "$1/healthz" -o /dev/null; do
    i=$((i + 1)); [ $i -gt 120 ] && return 1; sleep 0.25
  done
}
json_escape() { python3 -c 'import sys,json;sys.stdout.write(json.dumps(sys.stdin.read()))'; }
# 脚本任务一律定向到 agent：默认 HEALTH_FIRST 会在两个在线节点里挑，普通 demo worker 没有
# 脚本处理器，那样所有断言都变成掷骰子。
TAGGED='"dispatch_strategy":11,"dispatch_strategy_config":"tagEquals:executor"' 

# mkjob <任务名> <处理器> <任务参数> [额外 json 片段（不带前导逗号）]
# RETRY_JSON 单独抽出来，是因为 mkjob 的默认口径"重试一律 0"偶尔要被反过来用（重派那条），
# 而 JSON 里放两个同名键不可靠（后写的会不会赢取决于解析器）。要放开重试就改这个变量。
RETRY_JSON='"max_retry_times":0,"instance_retry_num":0'
mkjob() {
  local body
  # max_retry_times/instance_retry_num 默认 0：失败重试会清空 target_worker_id 换节点重跑，
  # 那样"内联被拒""退出码 3"这些断言会读到另一个节点那句不相干的失败原因。
  body=$(printf '{"app_id":"%s","job_name":"%s","processor":"%s","processor_type":"BUILT_IN","time_expression_type":"API",%s,"job_params":%s%s}' \
    "$APP" "$1" "$2" "$RETRY_JSON" "$(printf '%s' "$3" | json_escape)" "${4:+,$4}")
  api "$BASE" /openapi/job/create -H 'Content-Type: application/json' -d "$body"
}
# mkjob_typed <任务名> <处理器类型> <处理器>
mkjob_typed() {
  local body
  body=$(printf '{"app_id":"%s","job_name":"%s","processor":"%s","processor_type":"%s","time_expression_type":"API"}' \
    "$APP" "$1" "$3" "$2")
  api "$BASE" /openapi/job/create -H 'Content-Type: application/json' -d "$body"
}
job_field() { # job_field <任务名> <字段>
  api_get "$BASE" "/openapi/job/list?app_id=$APP&page_size=0" | python3 -c "
import sys,json
for j in json.load(sys.stdin)['data']['data'] or []:
    if j['job_name']=='$1':
        print(j.get('$2','')); break
"
}
# run <任务名> [实例参数] → 打印新实例 id
run() {
  local jid inst i=0 body extra=""
  jid=$(job_field "$1" id)
  if [ -n "${2:-}" ]; then
    extra=$(printf ',"instance_params":%s' "$(printf '%s' "$2" | json_escape)")
  fi
  body=$(printf '{"job_id":%s,"action":"trigger"%s}' "$jid" "$extra")
  api "$BASE" /openapi/job/operate -H 'Content-Type: application/json' -d "$body" >/dev/null
  while [ $i -lt 160 ]; do
    inst=$(api_get "$BASE" "/openapi/instance/list?job_id=$jid" \
      | python3 -c "import sys,json;d=json.load(sys.stdin)['data'] or [];print(d[0]['id'] if d else '')")
    [ -n "$inst" ] && { printf '%s' "$inst"; return; }
    i=$((i + 1)); sleep 0.25
  done
  printf '0'
}
# run-wait <job_id> → 该 job 最新实例 id（任务已触发过，只等它出现）
run-wait() {
  local inst i=0
  while [ $i -lt 160 ]; do
    inst=$(api_get "$BASE" "/openapi/instance/list?job_id=$1" \
      | python3 -c "import sys,json;d=json.load(sys.stdin)['data'] or [];print(d[0]['id'] if d else '')")
    [ -n "$inst" ] && { printf '%s' "$inst"; return; }
    i=$((i + 1)); sleep 0.25
  done
  printf '0'
}
# settle <实例 id> [等待轮数] → 终态状态
settle() {
  local inst=$1 patience=${2:-160} i=0 st
  while [ $i -lt "$patience" ]; do
    st=$(jget "$(api_get "$BASE" "/openapi/instance/get?instance_id=$inst")" "root['data']['status']")
    case "$st" in
      SUCCEED | FAILED | STOPPED) printf '%s' "$st"; return ;;
    esac
    i=$((i + 1)); sleep 0.25
  done
  printf 'TIMEOUT(%s)' "$st"
}
task_of() { # task_of <实例 id> <字段>
  api_get "$BASE" "/openapi/instance/tasks?instance_id=$1" | python3 -c "
import sys,json
d=json.load(sys.stdin).get('data') or [{}]
print(d[0].get('$2',''))"
}
logs_of() { # 该实例首个任务的在线日志全文
  local tid
  tid=$(task_of "$1" id)
  api_get "$BASE" "/openapi/instance/logs?task_id=$tid&limit=2000"
}
contains() { # contains <说明> <期望子串> <全文>
  case "$3" in
    *"$2"*) ok "$1" ;;
    *) bad "$1 —— 没找到「$2」，实际：$(printf '%.400s' "$3")" ;;
  esac
}
want_status() { # want_status <说明> <期望终态> <实例 id> [等待轮数]
  local st
  st=$(settle "$3" "${4:-160}")
  if [ "$st" = "$2" ]; then ok "$1（$st）"; else bad "$1 —— 期望 $2，实得 $st，结果列：$(task_of "$3" result)"; fi
}

echo "== 0. 构建"
cd "$(dirname "$0")/.." || exit 1
$GO build -o "$SBIN" ./cmd/gojob-server || { bad "编译 server 失败"; exit 1; }
$GO build -o "$ABIN" ./cmd/gojob-agent || { bad "编译 agent 失败"; exit 1; }
$GO build -o "$WBIN" ./cmd/gojob-demo || { bad "编译 demo worker 失败"; exit 1; }
ok "编译 gojob-server / gojob-agent / gojob-demo"

echo
echo "== 1. 起 server、脚本库、agent 与一个没有脚本处理器的普通 worker"
mkdir -p "$LIB"
cat >"$LIB/ok.sh" <<'EOS'
#!/bin/sh
echo "库脚本开始，参数=$1"
echo "一行标准错误" >&2
echo "库脚本完成"
EOS
cat >"$LIB/fail3.sh" <<'EOS'
#!/bin/sh
echo "先正常输出一行"
echo "失败原因在 stderr" >&2
exit 3
EOS
# 处理器把工作目录设成脚本所在目录，所以 leak.pid 落在脚本库里
cat >"$LIB/leak.sh" <<'EOS'
#!/bin/sh
sleep 60 &
echo $! > leak.pid
echo "已启动后台子进程"
wait
EOS
chmod 755 "$LIB"/*.sh

( GOJOB_CLUSTER_SECRET=$SECRET GOJOB_ADDR=:"$PORT" GOJOB_PUBLIC_URL=$BASE \
  GOJOB_DB_DRIVER=sqlite GOJOB_DB_DSN=$DB "$SBIN" >"$WORK/server.log" 2>&1 &
  echo $! >"$WORK/server.pid" )
wait_health "$BASE" || { bad "server 没起来"; tail -20 "$WORK/server.log"; exit 1; }
ok "server 就绪 :$PORT"
login "$BASE"

BODY=$(printf '{"app_id":"%s","app_name":"脚本执行验收","password":"%s"}' "$APP" "$PW")
want_code=$(jget "$(api "$BASE" /openapi/app/create -H 'Content-Type: application/json' -d "$BODY")" "root['code']")
[ "$want_code" = "0" ] && ok "创建项目 $APP" || { bad "创建项目失败 code=$want_code"; exit 1; }

start_agent() { # start_agent [额外环境变量赋值，形如 A=1 B=2]
  # shellcheck disable=SC2086
  ( env GOJOB_WORKER_SERVER=$BASE GOJOB_WORKER_APP=$APP GOJOB_WORKER_PASSWORD=$PW \
      GOJOB_WORKER_TAG=executor GOJOB_AGENT_SCRIPT_DIR=$LIB \
      GOJOB_AGENT_MAX_CONCURRENCY=4 $1 "$ABIN" >>"$AGENT_LOG" 2>&1 &
    echo $! >"$AGENT_PID_FILE" )
}
start_agent ""
sleep 7 # 等两跳心跳：注册与在线节点都落库了才能派单
ok "agent 已上线（tag=executor，脚本库=$LIB）"
# 第 2~4、6~9 节都只有这一个执行节点：失败任务的重试会清空 target_worker_id 换节点重跑，
# 多一个节点在，失败原因那几条断言就会读到别的节点报的句子。

echo
echo "== 2. 脚本库模式：短名与官方类全名等价，参数能进脚本"
mkjob lib-short shell "ok.sh world" "$TAGGED" >/dev/null
I=$(run lib-short)
want_status "短名 shell 的库脚本任务成功" SUCCEED "$I"
contains "脚本参数传到了脚本里" "参数=world" "$(logs_of "$I")"
contains "脚本 stdout 进了在线日志" "库脚本完成" "$(logs_of "$I")"
contains "脚本 stderr 也进了在线日志" "一行标准错误" "$(logs_of "$I")"

FQ='tech.powerjob.official.processors.impl.script.ShellProcessor'
mkjob lib-fq "$FQ" "ok.sh fqcn" "$TAGGED" >/dev/null
I2=$(run lib-fq)
want_status "官方类全名与短名走同一个处理器" SUCCEED "$I2"
contains "类全名任务的参数同样传进去了" "参数=fqcn" "$(logs_of "$I2")"
ok "同一份任务配置在两边写法一致（processorInfo=类全名 + 脚本放任务参数）"

echo
echo "== 3. 默认不执行任务参数里直接给的脚本文本"
INLINE='echo 不该被执行 > /dev/null'
mkjob inline-deny shell "$INLINE" "$TAGGED" >/dev/null
I3=$(run inline-deny)
want_status "内联脚本默认被拒" FAILED "$I3"
contains "拒绝原因点名了那个开关" "GOJOB_AGENT_ALLOW_INLINE_SCRIPTS" "$(task_of "$I3" result)"

echo
echo "== 4. 退出码与失败输出"
mkjob fail3 shell "fail3.sh" "$TAGGED" >/dev/null
I4=$(run fail3)
want_status "非 0 退出码判失败" FAILED "$I4"
contains "结果里带退出码" "退出码 3" "$(task_of "$I4" result)"
contains "结果里带 stderr 尾巴" "失败原因在 stderr" "$(task_of "$I4" result)"
contains "在线日志里有脚本的正常输出" "先正常输出一行" "$(logs_of "$I4")"

echo
echo "== 5. SPECIFY 标签定向：脚本只落在 agent 上"
# 这里才把普通 demo worker 拉起来：它没有脚本处理器、但注册了 hello
( GOJOB_WORKER_SERVER=$BASE GOJOB_WORKER_APP=$APP GOJOB_WORKER_PASSWORD=$PW \
  "$WBIN" >"$WORKER_LOG" 2>&1 &
  echo $! >"$WORK/worker.pid" )
sleep 7
mkjob specify shell "ok.sh routed" "$TAGGED" >/dev/null
I5=$(run specify)
want_status "指定 executor 标签的脚本任务成功" SUCCEED "$I5"
contains "指定标签这次真的执行了脚本" "参数=routed" "$(logs_of "$I5")"
# 反向证据：把一个只有 demo worker 认识的处理器（hello）定向派给 agent。
# 预选生效的话，第一跳一定落在 agent 上，并由 agent 报"没注册"——demo worker 抢不走第一跳。
# 这一条要放开重试才谈得上"换节点"，所以临时改 RETRY_JSON，用完立刻改回去。
RETRY_JSON='"max_retry_times":3,"instance_retry_num":0'
mkjob misroute hello "ignored" "$TAGGED" >/dev/null
RETRY_JSON='"max_retry_times":0,"instance_retry_num":0'
# 顺带守住"重试次数按请求落库"：gorm 的 Create 会跳过带 default 标签的零值列，
# 曾经 max_retry_times=0 被库里的 DEFAULT 3 静默顶掉（=3 也可能被顶成别的值），
# 而 Create 之后内存里的结构体仍显示请求值，所以只在实机 + 读回库里才看得见。
[ "$(job_field specify max_retry_times)" = "0" ] &&
  ok "max_retry_times=0 真的落库（不重试）" ||
  bad "0 被 DB 默认值顶掉了：实得 $(job_field specify max_retry_times)"
[ "$(job_field misroute max_retry_times)" = "3" ] &&
  ok "max_retry_times=3 同样按请求落库" ||
  bad "请求的 3 没落库：实得 $(job_field misroute max_retry_times)"
J5B=$(job_field misroute id)
api "$BASE" /openapi/job/operate -H 'Content-Type: application/json' -d "$(printf '{"job_id":%s,"action":"trigger"}' "$J5B")" >/dev/null
i5=0
until grep -q 'processor "hello" not registered' "$AGENT_LOG" 2>/dev/null || [ $i5 -ge 40 ]; do
  i5=$((i5 + 1)); sleep 0.25
done
if grep -q 'processor "hello" not registered' "$AGENT_LOG" 2>/dev/null; then
  ok "标签预选生效：第一跳由 agent 执行并报「没注册该处理器」"
else
  bad "agent 没接到这一跳（预选没锁住节点）"
fi
# 既定语义：任务级重试会清空预选（target_worker_id 归零），于是重派有机会落到别的节点——
# "失败重派优先保运行"，见 worker README。断言落在可确定的那半边：放开没放开。
# 至于最终这一跳被谁捡走，那是两台节点抢同一行的赛跑（实测 4 次尝试能在 9ms 内全落在
# 同一台），拿它当断言会随机红。
M5=$(run-wait "$J5B")
settle "$M5" 160 >/dev/null
ATTEMPTS=$(task_of "$M5" attempts)
PINNED=$(task_of "$M5" target_worker_id)
if [ "${ATTEMPTS:-1}" -gt 1 ] && [ -z "$PINNED" ]; then
  ok "失败重派会换节点：共尝试 $ATTEMPTS 次、预选已放开"
else
  bad "重派没放开预选（attempts=${ATTEMPTS:-?} target_worker_id=$PINNED）"
fi
echo
echo "== 7. 停止实例会连脚本自己起的后台子进程一起收"
rm -f "$LIB/leak.pid"
mkjob leak shell "leak.sh" "$TAGGED" >/dev/null
I7=$(run leak)
sleep 3
LEAK_PID=$(cat "$LIB/leak.pid" 2>/dev/null || echo "")
[ -n "$LEAK_PID" ] && ok "脚本已启动后台子进程 pid=$LEAK_PID" || bad "脚本没能写出 leak.pid"
BODY=$(printf '{"instance_id":%s,"app_id":"%s"}' "$I7" "$APP")
want_code=$(jget "$(api "$BASE" /openapi/instance/stop -H 'Content-Type: application/json' -d "$BODY")" "root['code']")
[ "$want_code" = "0" ] && ok "停止实例指令已下发" || bad "停止实例失败 code=$want_code"
want_status "脚本任务被停止" STOPPED "$I7" 120
if [ -n "$LEAK_PID" ]; then
  sleep 1
  if kill -0 "$LEAK_PID" 2>/dev/null; then
    bad "后台子进程 $LEAK_PID 变成孤儿还在跑"
  else
    ok "后台子进程随进程组一起被收走（$LEAK_PID 已退出）"
  fi
fi

echo
echo "== 8. 显式开内联开关后，同一条任务就能跑"
kill "$(cat "$AGENT_PID_FILE")" 2>/dev/null # TERM 走优雅下线，别在输出里留 Killed: 9
sleep 2
start_agent "GOJOB_AGENT_ALLOW_INLINE_SCRIPTS=true"
# 被杀掉的旧 agent 在 worker_nodes 里还会"在线"最多 15s（判活阈值）。这窗口内按标签预选可能
# 选中那个已经不存在的节点，任务要等 ReleaseStaleTargets(60s) 才被放行，所以等过窗口再断言。
sleep 18
want_status "内联脚本在显式授权后执行成功" SUCCEED "$(run inline-deny)" 200
contains "开开关时 agent 打了醒目告警日志" "任何能建任务" "$(cat "$AGENT_LOG")"

echo
echo "== 9. 导入与 OpenAPI 建单对处理器类型的处理"
IMPORT=$(printf '{"app_id":"%s","jobs":[{"jobName":"legacy-shell","timeExpressionType":"API","processorType":"SHELL","processorInfo":"echo 旧 SHELL 任务的脚本正文","executeType":"STANDALONE","dispatchStrategy":11,"dispatchStrategyConfig":"tagEquals:executor"},{"jobName":"legacy-python","timeExpressionType":"API","processorType":"PYTHON","processorInfo":"print(1)","executeType":"STANDALONE"},{"jobName":"legacy-external","timeExpressionType":"API","processorType":"EXTERNAL","processorInfo":"1#com.x.Foo","executeType":"STANDALONE"}]}' "$APP")
RESP=$(api "$BASE" /openapi/job/import -H 'Content-Type: application/json' -d "$IMPORT")
item() { printf '%s' "$RESP" | python3 -c "
import sys,json
for i in json.load(sys.stdin)['data']['items']:
    if i['job_name']=='$1': print(i)
"; }
contains "旧的 SHELL 任务导入成功" "'success': True" "$(item legacy-shell)"
[ "$(job_field legacy-shell processor)" = "$FQ" ] &&
  ok "处理器改写为官方脚本处理器类全名" || bad "processor=$(job_field legacy-shell processor)"
[ "$(job_field legacy-shell processor_type)" = "BUILT_IN" ] &&
  ok "处理器类型改写为 BUILT_IN" || bad "processor_type=$(job_field legacy-shell processor_type)"
contains "脚本文本搬到了任务参数" "旧 SHELL 任务的脚本正文" "$(job_field legacy-shell job_params)"
contains "PYTHON 逐条给出原因" "PYTHON" \
  "$(printf '%s' "$RESP" | python3 -c "import sys,json;print([i for i in json.load(sys.stdin)['data']['items'] if i['job_name']=='legacy-python'])")"
contains "EXTERNAL 逐条给出原因（不当哑弹收下）" "EXTERNAL" \
  "$(printf '%s' "$RESP" | python3 -c "import sys,json;print([i for i in json.load(sys.stdin)['data']['items'] if i['job_name']=='legacy-external'])")"
[ "$(job_field legacy-python id)" = "" ] && ok "被拒的导入没有落库" || bad "被拒的任务仍然落库了"
want_status "改写后的旧 SHELL 任务真的能跑" SUCCEED "$(run legacy-shell)" 160

echo
echo "==================== 通过 $pass 项 / 失败 $fail 项 ===================="
[ "$fail" = "0" ] || exit 1
