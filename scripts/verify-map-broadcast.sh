#!/usr/bin/env bash
# gojob 多任务执行类型（BROADCAST / MAP / MAP_REDUCE）实机验收：起 1 个 server + 2 个 demo worker，
# 把"一个实例一棵任务树"这条链路真跑一遍：
#   ① BROADCAST：在线节点每台恰好一份、落在不同节点上、层次号 0.1/0.2，根任务自己不占执行节点；
#   ② BROADCAST 落在零个在线节点上：当场收敛，不留一个永远等不到回报的实例；
#   ③ MAP：根任务运行期派生子任务，树全部收尾实例才进终态，实例的任务进度列跟着涨；
#   ④ MAP 多级派生：子任务还能继续 map（兼容协议同语义），层次号长成 0.1.2；
#   ⑤ MAP：任一子任务最终失败 ⇒ 实例失败（兼容协议那句 success = failedNum == 0）；
#   ⑥ MAP_REDUCE：树排空之后才补出 reduce 任务（OMS_LAST_TASK，层次号 9999），它的返回值就是实例结果；
#   ⑦ MAP_REDUCE 容忍部分失败：reduce 兜住就 SUCCEED —— ⑤ 与 ⑦ 合起来才是 MAP 与 MAP_REDUCE 的分界；
#   ⑧ MAP_REDUCE：reduce 自己判失败 ⇒ 实例失败，结果取 reduce 那句错误；
#   ⑨ 停止实例：未派发的当场终止、运行中的协作退出，而且**不会**再补一个 reduce 任务；
#   ⑩ 父任务重派：旧一代未收敛的后代置 DISCARDED，新一代照常派得出来（派生幂等键带尝试次数）；
#   ⑪ STANDALONE 实例里调 map 被明确拒绝：执行类型是真的闸，不是页面摆设；
#   ⑫ 实例详情接口：Task 统计 + 控制台那句 "select * from task_info where … limit 10"，注入写法被挡在白名单外；
#   ⑬ 建单对未实现执行类型（SHARDING/未知值）当条给出可操作原因，导入仍用执行类型名字符串。
#
# 一条既定语义没在这里演：广播子任务在执行节点失联时判失败、不换机重派（兼容协议 issue #1003）。
# 它的判活阈值是 5 分钟，实机等不起，由 server/dispatch/multitask_test.go 覆盖。
#
# 用法: scripts/verify-map-broadcast.sh      （可用 GO/PORT/APP/PW/SECRET 覆盖）
# 注意 JSON 一律先 printf 拼进变量再交给 curl：macOS 自带 bash 3.2 在双引号里嵌套 $()
# 再套转义引号时解析会错乱，别在这里踩。
set -u
GO=${GO:-go}
PORT=${PORT:-7891}
BASE=http://127.0.0.1:$PORT
APP_MAIN=${APP:-mb_app}
APP=$APP_MAIN
DEAD_APP=${DEAD_APP:-mb_empty} # 只有项目、没有任何在线节点的项目，用来演广播落在零台机器上
PW=${PW:-mb123}
SECRET=${SECRET:-map-broadcast-secret}
WORK=$(mktemp -d)
DB=$WORK/mb.db
SBIN=$WORK/gojob-server
WBIN=$WORK/gojob-demo
W1_LOG=$WORK/worker1.log
W2_LOG=$WORK/worker2.log
pass=0
fail=0
TOKEN=
# 重试默认关死：⑤⑧ 要的是"这一片最终失败"，让它重试三次只是把同一个结论晚几分钟给出。
# 第 10 节要的就是重派，那里临时把 RETRY 换掉（用变量而不是往 JSON 里塞两个同名键）。
RETRY='"max_retry_times":0,"instance_retry_num":0'

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
api() { local base=$1 path=$2; shift 2; curl -s -m 20 -X POST "$base$path" -H "X-GoJob-Token: $TOKEN" "$@"; }
api_get() { curl -s -m 20 "$1$2" -H "X-GoJob-Token: $TOKEN"; }
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

# mkjob <任务名> <处理器> <任务参数> <执行类型>
mkjob() {
  local body
  body=$(printf '{"app_id":"%s","job_name":"%s","processor":"%s","processor_type":"BUILT_IN","execute_type":"%s","time_expression_type":"API",%s,"job_params":%s}' \
    "$APP" "$1" "$2" "$4" "$RETRY" "$(printf '%s' "$3" | json_escape)")
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
run() { # run <任务名> → 新实例 id
  local jid inst i=0
  jid=$(job_field "$1" id)
  api "$BASE" /openapi/job/operate -H 'Content-Type: application/json' \
    -d "$(printf '{"job_id":%s,"action":"trigger"}' "$jid")" >/dev/null
  while [ $i -lt 160 ]; do
    inst=$(api_get "$BASE" "/openapi/instance/list?job_id=$jid" \
      | python3 -c "import sys,json;d=json.load(sys.stdin)['data'] or [];print(d[0]['id'] if d else '')")
    [ -n "$inst" ] && { printf '%s' "$inst"; return; }
    i=$((i + 1)); sleep 0.25
  done
  printf '0'
}
settle() { # settle <实例 id> [等待轮数] → 终态状态
  local inst=$1 patience=${2:-240} i=0 st
  while [ $i -lt "$patience" ]; do
    st=$(jget "$(api_get "$BASE" "/openapi/instance/get?instance_id=$inst")" "root['data']['status']")
    case "$st" in
      SUCCEED | FAILED | STOPPED) printf '%s' "$st"; return ;;
    esac
    i=$((i + 1)); sleep 0.25
  done
  printf 'TIMEOUT(%s)' "$st"
}
inst_field() { # inst_field <实例 id> <字段>
  jget "$(api_get "$BASE" "/openapi/instance/get?instance_id=$1")" "root['data'].get('$2','')"
}
tasks_raw() { api_get "$BASE" "/openapi/instance/tasks?instance_id=$1"; }
tcalc() { # tcalc <实例 id> <python 表达式，ts 是该实例全部任务行>
  tasks_raw "$1" | python3 -c "
import sys,json
ts=json.load(sys.stdin).get('data') or []
try: print($2)
except Exception as e: print('expr-error',e)
"
}
# dp <实例 id> <查询串> → 实例详情接口（兼容协议的 detailPlus 同位）的原始响应
dp() {
  local body
  body=$(printf '{"instance_id":%s,"app_id":"%s","custom_query":%s}' \
    "$1" "$APP" "$(printf '%s' "${2:-}" | json_escape)")
  api "$BASE" /openapi/instance/detailPlus -H 'Content-Type: application/json' -d "$body"
}
qtotal() { jget "$(dp "$1" "$2")" "root['data']['total']"; }
rootid() { tcalc "$1" "[t for t in ts if t['parent_task_id']==0][0]['id']"; }
rootres() { tcalc "$1" "[t for t in ts if t['parent_task_id']==0][0]['result']"; }
online_ids() { # 当前在线节点的 instance_id，空格分隔、已排序
  jget "$(api_get "$BASE" "/openapi/app/workers?app_id=$APP")" \
    "' '.join(sorted(w['instance_id'] for w in root['data'] or []))"
}
contains() { # contains <说明> <期望子串> <全文>
  case "$3" in
    *"$2"*) ok "$1" ;;
    *) bad "$1 —— 没找到「$2」，实际：$(printf '%.400s' "$3")" ;;
  esac
}
eq() { # eq <说明> <期望> <实际>
  if [ "$2" = "$3" ]; then ok "$1（$3）"; else bad "$1 —— 期望 $2，实得 $3"; fi
}
ge() { # ge <说明> <下限> <实际>
  if [ "${3:-x}" -ge "$2" ] 2>/dev/null; then ok "$1（$3）"; else bad "$1 —— 期望 ≥$2，实得 $3"; fi
}
want_status() { # want_status <说明> <期望终态> <实例 id> [等待轮数]
  local st
  st=$(settle "$3" "${4:-240}")
  if [ "$st" = "$2" ]; then ok "$1（$st）"; else bad "$1 —— 期望 $2，实得 $st，实例结果：$(inst_field "$3" result)"; fi
}

echo "== 0. 构建"
cd "$(dirname "$0")/.." || exit 1
$GO build -o "$SBIN" ./cmd/gojob-server || { bad "编译 server 失败"; exit 1; }
$GO build -o "$WBIN" ./cmd/gojob-demo || { bad "编译 demo worker 失败"; exit 1; }
ok "编译 gojob-server / gojob-demo"

echo
echo "== 1. 起 server 与两个在线执行节点"
( GOJOB_CLUSTER_SECRET=$SECRET GOJOB_ADDR=:"$PORT" GOJOB_PUBLIC_URL=$BASE \
  GOJOB_DB_DRIVER=sqlite GOJOB_DB_DSN=$DB "$SBIN" >"$WORK/server.log" 2>&1 &
  echo $! >"$WORK/server.pid" )
wait_health "$BASE" || { bad "server 没起来"; tail -20 "$WORK/server.log"; exit 1; }
ok "server 就绪 :$PORT"
login "$BASE"

BODY=$(printf '{"app_id":"%s","app_name":"多任务验收","password":"%s"}' "$APP" "$PW")
[ "$(jget "$(api "$BASE" /openapi/app/create -H 'Content-Type: application/json' -d "$BODY")" "root['code']")" = "0" ] &&
  ok "创建项目 $APP" || { bad "创建项目 $APP 失败"; exit 1; }
BODY=$(printf '{"app_id":"%s","app_name":"没有节点的验收项目","password":"%s"}' "$DEAD_APP" "$PW")
[ "$(jget "$(api "$BASE" /openapi/app/create -H 'Content-Type: application/json' -d "$BODY")" "root['code']")" = "0" ] &&
  ok "创建空项目 $DEAD_APP" || bad "创建空项目 $DEAD_APP 失败"

start_worker() { # start_worker <日志文件>
  ( GOJOB_WORKER_SERVER=$BASE GOJOB_WORKER_APP=$APP GOJOB_WORKER_PASSWORD=$PW \
      "$WBIN" >>"$1" 2>&1 &
    echo $! >"$1.pid" )
}
start_worker "$W1_LOG"
start_worker "$W2_LOG"
sleep 7 # 两跳心跳：注册与在线节点都落库了，广播才知道该派给谁
NODES=$(online_ids)
eq "两个 worker 都上线" "2" "$(printf '%s' "$NODES" | wc -w | tr -d ' ')"

echo
echo "== 2. BROADCAST：在线节点每台恰好一份"
mkjob bc broadcastEcho ignored BROADCAST >/dev/null
I=$(run bc)
want_status "广播实例整体成功" SUCCEED "$I"
eq "实例上记着执行类型" "BROADCAST" "$(inst_field "$I" execute_type)"
eq "广播子任务份数 == 在线节点数" "2" "$(tcalc "$I" "len([t for t in ts if t['task_name']=='OMS_BROADCAST_TASK'])")"
eq "两份落在两个不同节点上，且就是当前在线的那两台" "$NODES" \
  "$(tcalc "$I" "' '.join(sorted({t['worker_id'] for t in ts if t['task_name']=='OMS_BROADCAST_TASK'}))")"
eq "层次号按 0.1 / 0.2 编排" "0.1 0.2" \
  "$(tcalc "$I" "' '.join(sorted(t['task_key'] for t in ts if t['task_name']=='OMS_BROADCAST_TASK'))")"
eq "广播子任务是指名到机器的（预选 == 实际执行节点）" "True" \
  "$(tcalc "$I" "all(t['target_worker_id']==t['worker_id'] for t in ts if t['task_name']=='OMS_BROADCAST_TASK')")"
eq "两份都成功" "2" \
  "$(tcalc "$I" "len([t for t in ts if t['task_name']=='OMS_BROADCAST_TASK' and t['status']=='SUCCEED'])")"
contains "每台真的各自跑了处理器（结果里带本机标识）" "node=" \
  "$(tcalc "$I" "'|'.join(t['result'] for t in ts if t['task_name']=='OMS_BROADCAST_TASK')")"
eq "根任务自己不占执行节点" "" "$(tcalc "$I" "[t for t in ts if t['parent_task_id']==0][0]['worker_id']")"
eq "根任务建出来就是终态" "SUCCEED" "$(tcalc "$I" "[t for t in ts if t['parent_task_id']==0][0]['status']")"
contains "实例结果写明广播了几台" "广播 2 个执行节点" "$(inst_field "$I" result)"
eq "实例任务总数含根任务" "3" "$(inst_field "$I" total_task_num)"
eq "收敛后不留未结束任务" "0" "$(inst_field "$I" running_times)"

echo
echo "== 2.5 控制台停掉一台节点的接单：广播的份数立刻少一台，且 worker 刷不回去"
# 「停止接单」就是 worker_nodes.accept_tasks 这一个字段，生效靠选节点时过滤：
# 被停的节点照常心跳/长轮询，只是认领 SQL 筛不到属于它的任务。
# 这一节要钉住三件事：① 停掉之后广播少一份；② 那台节点的心跳不能把开关刷回 1
# （在线表每 5s upsert 一次，若 accept_tasks 进了覆盖列，运维刚点的开关会被 worker 自己抹掉）；
# ③ 项目口令换来的兼容层票据调不到这个接口——它是管理面动作，只认控制台会话票。
DRAINED=$(printf '%s' "$NODES" | cut -d' ' -f1)
KEEP=$(printf '%s' "$NODES" | cut -d' ' -f2)
drain() { # drain <true|false> —— 翻被停那台节点的开关，回响应信封
  BODY=$(printf '{"app_id":"%s","instance_id":"%s","accept":%s}' "$APP_MAIN" "$DRAINED" "$1")
  api "$BASE" /openapi/worker/acceptTasks -H 'Content-Type: application/json' -d "$BODY"
}
accepted() { # accepted <instance_id> —— 该节点当前的接单开关（列表接口读回来）
  api_get "$BASE" "/openapi/app/workers?app_id=$APP_MAIN" | python3 -c "
import sys,json
want='$1'
for w in json.load(sys.stdin)['data'] or []:
    if w['instance_id'] == want: print(w['accept_tasks'])
"
}
CODE=$(jget "$(drain false)" "root['code']"); eq "停止接单成功" "0" "$CODE"
eq "节点列表里它还在（否则开关点不回来）" "2" "$(printf '%s' "$NODES" | wc -w | tr -d ' ')"
eq "它被标成已停止接单" "False" "$(accepted "$DRAINED")"
sleep 11 # 让它自己再跳两跳心跳
eq "两跳心跳之后仍是停止接单" "False" "$(accepted "$DRAINED")"
I25=$(run bc); settle "$I25" 240 >/dev/null
eq "广播只发给还在接单的那一台" "1" \
  "$(tcalc "$I25" "len([t for t in ts if t['task_name']=='OMS_BROADCAST_TASK'])")"
eq "被停的那台一份都没拿到" "False" \
  "$(tcalc "$I25" "any(t.get('worker_id')=='$DRAINED' for t in ts)")"
eq "拿到那一份的是没被停的那台" "$KEEP" \
  "$(tcalc "$I25" "[t['worker_id'] for t in ts if t['task_name']=='OMS_BROADCAST_TASK'][0]")"
contains "结果里如实说明只广播了 1 台" "广播 1 个执行节点" "$(inst_field "$I25" result)"
# 错误码在响应体的 code 里（HTTP 恒 200），所以读 body
PJT=$(curl -s -m5 -X POST $BASE/openApi/authApp -H 'Content-Type: application/json' \
  -d "{\"appName\":\"$APP_MAIN\",\"password\":\"$PW\"}" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
BODY=$(printf '{"app_id":"%s","instance_id":"%s","accept":true}' "$APP_MAIN" "$DRAINED")
CODE=$(curl -s -m5 -X POST "$BASE/openapi/worker/acceptTasks" -H "X-GoJob-Token: $PJT" \
  -H 'Content-Type: application/json' -d "$BODY" | sed -n 's/.*"code":\([0-9]*\).*/\1/p')
eq "项目口令的票据调不到停止接单" "401" "$CODE"
CODE=$(jget "$(drain true)" "root['code']"); eq "恢复接单成功" "0" "$CODE"
eq "开关翻回接单中" "True" "$(accepted "$DRAINED")"
I26=$(run bc); settle "$I26" 240 >/dev/null
eq "恢复之后广播回到两份" "2" \
  "$(tcalc "$I26" "len([t for t in ts if t['task_name']=='OMS_BROADCAST_TASK'])")"

echo
echo "== 3. BROADCAST 落在零个在线节点上：当场收敛，不挂死"
APP=$DEAD_APP
mkjob bc-empty broadcastEcho ignored BROADCAST >/dev/null
I3=$(run bc-empty)
want_status "没有节点时广播不会永远等回报" SUCCEED "$I3"
contains "结果里如实说明没执行" "广播未执行" "$(inst_field "$I3" result)"
eq "未结束任务数归零" "0" "$(inst_field "$I3" running_times)"
mkjob bc-again broadcastEcho ignored BROADCAST >/dev/null
eq "空项目里这棵树一行不落执行节点，只有根" "1" "$(tcalc "$(run bc-again)" "len(ts)")"
APP=$APP_MAIN

echo
echo "== 4. MAP：根任务运行期派生子任务"
mkjob split mapSplit "alpha,beta,gamma" MAP >/dev/null
I4=$(run split)
want_status "Map 实例成功" SUCCEED "$I4"
eq "树形：根 + 3 片" "4" "$(tcalc "$I4" "len(ts)")"
eq "子任务名取 map 的第二参" "UPPER" "$(tcalc "$I4" "sorted({t['task_name'] for t in ts if t['parent_task_id']!=0})[0]")"
eq "层次号 0.1/0.2/0.3" "0.1 0.2 0.3" \
  "$(tcalc "$I4" "' '.join(sorted(t['task_key'] for t in ts if t['parent_task_id']!=0))")"
eq "每片拿到自己的负载并各自执行" "ALPHA BETA GAMMA" \
  "$(tcalc "$I4" "' '.join(sorted(t['result'] for t in ts if t['parent_task_id']!=0))")"
contains "子任务负载原样落库可查" "word" "$(tcalc "$I4" "'|'.join(t['sub_task_content'] for t in ts if t['parent_task_id']!=0)")"
contains "根任务把派生了几个写进结果" "已派生 3 片" "$(rootres "$I4")"
eq "派生后实例任务数立刻涨到 4（控制台任务进度列读它）" "4" "$(inst_field "$I4" total_task_num)"
eq "实例收敛后计数为 0 未结束" "0" "$(inst_field "$I4" running_times)"

echo
echo "== 5. MAP 多级派生：子任务还能继续 map"
mkjob deep deepMap "2,3" MAP >/dev/null
I5=$(run deep)
want_status "多级 Map 实例成功" SUCCEED "$I5"
eq "树形：根 1 + 组 2 + 项 6" "9" "$(tcalc "$I5" "len(ts)")"
eq "中间层各自收敛" "2" "$(tcalc "$I5" "len([t for t in ts if t['task_name']=='GROUP' and t['status']=='SUCCEED'])")"
eq "叶子结果齐全" "g1-i1 g1-i2 g1-i3 g2-i1 g2-i2 g2-i3" \
  "$(tcalc "$I5" "' '.join(sorted(t['result'] for t in ts if t['task_name']=='ITEM'))")"
contains "层次号带得出父子关系" "0.1.1" "$(tcalc "$I5" "' '.join(t['task_key'] for t in ts)")"
eq "第三层的父任务就是第二层那两行" "6" \
  "$(tcalc "$I5" "len([t for t in ts if t['parent_task_id'] in [x['id'] for x in ts if x['task_name']=='GROUP']])")"

echo
echo "== 6. MAP：任一子任务失败 ⇒ 实例失败"
mkjob failone mapFailOne "a,b,c,2" MAP >/dev/null
I6=$(run failone)
want_status "一片失败就把整个 Map 判失败" FAILED "$I6"
eq "实例计数里就是那一片失败" "1" "$(inst_field "$I6" failed_task_num)"
eq "另外两片仍然成功（失败不牵连别的片）" "2" \
  "$(tcalc "$I6" "len([t for t in ts if t['status']=='SUCCEED' and t['parent_task_id']!=0])")"
contains "实例结果取根任务那句（兼容协议取 root 的 ProcessResult）" "第 2 片会失败" "$(inst_field "$I6" result)"

echo
echo "== 7. MAP_REDUCE：树排空才补出 reduce，它的返回值就是实例结果"
mkjob mr-ok mrSum "1,2,3,4" MAP_REDUCE >/dev/null
I7=$(run mr-ok)
want_status "MapReduce 实例成功" SUCCEED "$I7"
eq "reduce 任务名是保留的 OMS_LAST_TASK" "1" "$(tcalc "$I7" "len([t for t in ts if t['task_name']=='OMS_LAST_TASK'])")"
eq "reduce 的层次号固定 9999" "9999" "$(tcalc "$I7" "[t['task_key'] for t in ts if t['task_name']=='OMS_LAST_TASK'][0]")"
eq "reduce 也带执行类型" "MAP_REDUCE" "$(tcalc "$I7" "[t['execute_type'] for t in ts if t['task_name']=='OMS_LAST_TASK'][0]")"
eq "reduce 真的在某台机器上跑过" "True" "$(tcalc "$I7" "bool([t['worker_id'] for t in ts if t['task_name']=='OMS_LAST_TASK'][0])")"
eq "树含根 + 4 片 + reduce" "6" "$(tcalc "$I7" "len(ts)")"
eq "reduce 不在任何子任务之前执行（id 最大）" "True" \
  "$(tcalc "$I7" "max(t['id'] for t in ts)==[t['id'] for t in ts if t['task_name']=='OMS_LAST_TASK'][0]")"
eq "实例结果 == reduce 的返回值" "收到 4 片，成功 4，失败 0，总和=10" "$(inst_field "$I7" result)"

echo
echo "== 8. MAP_REDUCE 容忍部分失败（与第 6 节合起来才是 MAP / MAP_REDUCE 的分界）"
mkjob mr-bad mrSum "1,2,3,bad=2" MAP_REDUCE >/dev/null
I8=$(run mr-bad)
want_status "子任务失败但 reduce 兜住了 ⇒ 实例成功" SUCCEED "$I8"
eq "那一片确实判了失败" "1" "$(tcalc "$I8" "len([t for t in ts if t['status']=='FAILED'])")"
eq "reduce 拿到失败片并如实汇总" "收到 3 片，成功 2，失败 1，总和=4" "$(inst_field "$I8" result)"
mkjob mr-dead mrSum "5,bad=1" MAP_REDUCE >/dev/null
I9=$(run mr-dead)
want_status "reduce 自己判失败 ⇒ 实例失败" FAILED "$I9"
contains "实例结果取 reduce 那句错误而不是根任务的" "全部 1 个子任务都失败了" "$(inst_field "$I9" result)"

echo
echo "== 9. 停止实例：不补 reduce，未派发的当场终止、运行中的协作退出"
mkjob stop sleepMap "40,15" MAP_REDUCE >/dev/null
I10=$(run stop)
sleep 4
ge "停止前这棵树远没收敛" "30" "$(tcalc "$I10" "len([t for t in ts if t['status'] in ('WAITING','RUNNING')])")"
BODY=$(printf '{"instance_id":%s,"app_id":"%s"}' "$I10" "$APP")
eq "停止指令已下达" "0" "$(jget "$(api "$BASE" /openapi/instance/stop -H 'Content-Type: application/json' -d "$BODY")" "root['code']")"
want_status "实例停在 STOPPED" STOPPED "$I10"
eq "被停止的实例不会再补一个 reduce 任务" "0" "$(tcalc "$I10" "len([t for t in ts if t['task_name']=='OMS_LAST_TASK'])")"
eq "树里不再留未结束的行" "0" "$(tcalc "$I10" "len([t for t in ts if t['status'] in ('WAITING','RUNNING')])")"
contains "未派发的那批当场终止（不必等 worker）" "stopped before dispatch" \
  "$(tcalc "$I10" "'|'.join(t['result'] for t in ts)")"
contains "运行中的那批协作退出" "stopped by server request" \
  "$(tcalc "$I10" "'|'.join(t['result'] for t in ts)")"
eq "被停止不计入失败" "0" "$(inst_field "$I10" failed_task_num)"

echo
echo "== 10. 父任务重派：作废旧一代后代，新一代照常派生"
RETRY='"max_retry_times":2,"instance_retry_num":0'
mkjob retry retryMap "4,6" MAP >/dev/null
I11=$(run retry)
want_status "父任务重跑后实例仍能成功" SUCCEED "$I11"
eq "两代子任务都在库里（旧的留作旧账，新的重新算）" "8" "$(tcalc "$I11" "len([t for t in ts if t['task_name']=='RETRY'])")"
ge "旧一代未收敛的后代被作废（不重复执行副作用）" "1" "$(tcalc "$I11" "len([t for t in ts if t['status']=='DISCARDED'])")"
eq "作废不计入失败" "0" "$(inst_field "$I11" failed_task_num)"
eq "新一代四片全部成功" "4" "$(tcalc "$I11" "len([t for t in ts if t['task_name']=='RETRY' and t['status']=='SUCCEED'])")"
eq "父任务尝试次数记到 2" "2" "$(tcalc "$I11" "[t for t in ts if t['parent_task_id']==0][0]['attempts']")"
RETRY='"max_retry_times":0,"instance_retry_num":0'

echo
echo "== 11. STANDALONE 实例里调 map：明确拒绝"
mkjob standalone mapSplit "x,y" STANDALONE >/dev/null
I12=$(run standalone)
want_status "普通实例不允许派生子任务" FAILED "$I12"
contains "拒绝原因点出了本实例的执行类型" "STANDALONE" "$(rootres "$I12")"
eq "被拒的实例里没被塞进任何子任务" "1" "$(tcalc "$I12" "len(ts)")"

echo
echo "== 12. 实例详情接口：Task 统计 + task_info 查询"
D=$(dp "$I7" "")
eq "详情接口带上实例本身" "$I7" "$(jget "$D" "root['data']['instance']['id']")"
eq "Task 统计：总数" "6" "$(jget "$D" "root['data']['task_stats']['total']")"
eq "Task 统计：成功" "6" "$(jget "$D" "root['data']['task_stats']['succeed']")"
eq "Task 统计：未收敛" "0" "$(jget "$D" "root['data']['task_stats']['waiting']+root['data']['task_stats']['running']")"
eq "按状态查（兼容协议的序号 6=成功）" "6" "$(qtotal "$I7" 'status = 6')"
eq "按状态查（5=失败）" "0" "$(qtotal "$I7" 'status = 5')"
eq "按任务名查 reduce" "1" "$(qtotal "$I7" "task_name = 'OMS_LAST_TASK'")"
eq "按任务名查子任务片" "4" "$(qtotal "$I7" "task_name = 'NUM'")"
eq "多值 in 写法" "5" "$(qtotal "$I7" "task_name in ('OMS_ROOT_TASK', 'NUM')")"
eq "条件之间能用 and 连接" "1" "$(qtotal "$I7" "task_name = 'OMS_LAST_TASK' and status = 6")"
ROOT_ID=$(rootid "$I7")
eq "按父任务查" "4" "$(qtotal "$I7" "parent_task_id = $ROOT_ID")"
eq "合法排序照单收下" "6" "$(qtotal "$I7" 'status = 6 order by id desc')"
eq "limit 后缀照单收下（条数由服务端固定，与兼容协议同）" "6" "$(qtotal "$I7" 'status = 6 limit 10')"
# 详情弹窗一打开就是这句话（沿用兼容协议的默认查询）。它曾经报 no such column: last_modified_time——
# 排序列名是响应字段名，库里的列叫 modify_time，只有真查库才暴露，所以这里也钉一条。
eq "控制台默认那句真能查" "6" "$(qtotal "$I7" 'status in (5, 6) order by last_modified_time desc')"
BAD=$(dp "$I7" 'status = 6 order by result desc')
eq "排序列不在白名单里会被打回" "True" "$(jget "$BAD" "root['data']['query_error']!=''")"
contains "打回时说清楚支持什么写法" "支持的写法" "$BAD"
for inj in 'status = 6; drop table instance_tasks' "task_name = 'x' or 1=1" 'status = 6 --' \
  'union select result from instance_tasks' "task_name = 'x' /*"; do
  RESP=$(dp "$I7" "$inj")
  if [ "$(jget "$RESP" "root['data']['query_error']!=''")" = "True" ]; then
    ok "注入写法被白名单挡在门外：$(printf '%.40s' "$inj")"
  else
    bad "注入写法没被拦下：$inj → $(printf '%.200s' "$RESP")"
  fi
done
eq "被拦下时实例信息照常返回（详情弹窗不白屏）" "$I7" "$(jget "$RESP" "root['data']['instance']['id']")"
eq "任务直查接口与详情接口同源" "6" "$(tcalc "$I7" "len(ts)")"

echo
echo "== 13. 建单对未实现执行类型的口径"
RESP=$(mkjob sharding hello "" SHARDING)
eq "SHARDING 建不出来" "True" "$(jget "$RESP" "root['code']!=0")"
contains "拒绝 SHARDING 时给出替代方案" "请把任务改成 MAP" "$RESP"
contains "未知执行类型也被拒（不是悄悄回落单机）" "未知执行类型" "$(mkjob bogus hello "" MAPreduce)"
contains "工作流类型当条说明没有引擎" "WORKFLOW" "$(mkjob wf hello "" WORKFLOW)"
RESP=$(api "$BASE" /openapi/job/import -H 'Content-Type: application/json' \
  -d "$(printf '{"app_id":"%s","jobs":[{"jobName":"import-broadcast","timeExpressionType":"API","processorType":"BUILT_IN","processorInfo":"broadcastEcho","executeType":"BROADCAST","jobParams":"-"}]}' "$APP")")
contains "导入按执行类型名字符串收（与兼容协议导出格式一致）" "import-broadcast" "$RESP"
eq "导入的广播任务执行类型落库正确" "BROADCAST" "$(job_field import-broadcast execute_type)"
want_status "导入的广播任务真的能跑" SUCCEED "$(run import-broadcast)"

echo
echo "==================== 通过 $pass 项 / 失败 $fail 项 ===================="
[ "$fail" = "0" ] || exit 1
