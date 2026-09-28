#!/usr/bin/env bash
# 多副本（十三期）实机验收：两个 gojob-server 进程共用同一套库，验证
#   令牌跨副本互认 / 在线节点表跨副本共享 / app 级租约唯一归属 /
#   跨副本入队唤醒时延 / 冻结副本被接管 / 解冻后的僵尸主不会双跑。
#
# 本机没有 docker 与 mysql/pgsql，所以用 sqlite：WAL + _txlock=immediate + busy_timeout
# 是真跨进程锁，两个进程抢同一把租约的语义与 mysql 条件 UPDATE 等价。
#
# 注意 JSON 一律用 printf 先拼进变量：macOS 自带 bash 3.2 在「双引号里嵌套 $( ) 再套
# 转义引号」时解析会错乱（实测把一条 curl 拆成三条），别在这里踩。
# 用法: scripts/verify-cluster.sh   （可用 GO/A_PORT/B_PORT/APP/PW/SECRET 覆盖）
set -u
GO=${GO:-go}
A_PORT=${A_PORT:-7791}
B_PORT=${B_PORT:-7792}
A=http://127.0.0.1:$A_PORT
B=http://127.0.0.1:$B_PORT
APP=${APP:-clu_app}
PW=${PW:-clu123}
SECRET=${SECRET:-cluster-verify-secret}
WORK=$(mktemp -d)
DB=$WORK/cluster.db
BIN=$WORK/gojob-server
pass=0 fail=0
ok()  { pass=$((pass+1)); echo "  PASS  $1"; }
bad() { fail=$((fail+1)); echo "  FAIL  $1"; }
PIDS=()
PID_A=
PID_B=
TOKEN=
OWNER=
SUCCER=
OWNER_PID=
OWNER_LOG=
J_API=
J_3S=

cleanup() {
  for p in ${PIDS[@]+"${PIDS[@]}"}; do kill -9 "$p" 2>/dev/null; done
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

jget() { # jget <json> <python 表达式，root 为已解析对象>
  printf '%s' "$1" | python3 -c "
import sys,json
try: root=json.load(sys.stdin)
except Exception as e: print('parse-error',e); sys.exit()
try: print($2)
except Exception as e: print('missing',e)
"
}
want_code() { # want_code <说明> <期望> <json>
  got=$(jget "$3" "root['code']")
  if [ "$got" = "$2" ]; then ok "$1 (code=$got)"; else bad "$1 期望 code=$2 实得 $got -> $3"; fi
}
api() { # api <base> <path> [curl 参数...]  —— 自动带当前会话票
  local base=$1 path=$2
  shift 2
  curl -s -m 8 -X POST "$base$path" -H "X-Gojob-Token: $TOKEN" "$@"
}
api_get() { # api_get <base> <path>
  curl -s -m 8 "$1$2" -H "X-Gojob-Token: $TOKEN"
}
wapi() { # wapi <base> <path> <json body> —— 数据面：不带会话票，票在 body 里
  curl -s -m 8 -X POST "$1$2" -H 'Content-Type: application/json' -d "$3"
}
# lease_of <base> <app> -> "holder epoch alive"
lease_of() {
  api_get "$1" /openapi/system/nodes | python3 -c "
import sys,json
for l in json.load(sys.stdin)['data']['leases'] or []:
    if l['app_id']=='$2': print(l['holder'], l['epoch'], 1 if l['alive'] else 0)
"
}
# node_id_of <base> <监听地址>：把「谁持有租约」对回「哪个进程」
node_id_of() {
  api_get "$1" /openapi/system/nodes | python3 -c "
import sys,json
for s in json.load(sys.stdin)['data']['servers'] or []:
    if s['address']=='$2': print(s['node_id'])
"
}
inst_count() { # inst_count <base> <job id>
  api_get "$1" "/openapi/instance/list?job_id=$2" \
    | python3 -c "import sys,json;print(len(json.load(sys.stdin)['data'] or []))"
}
latest_status() { # latest_status <base> <job id>
  api_get "$1" "/openapi/instance/list?job_id=$2" \
    | python3 -c "import sys,json;d=json.load(sys.stdin)['data'] or [];print(d[0]['status'] if d else '')"
}
# 同一秒内被创建两次的实例对数：3s 周期下出现即为双跑
dup_pairs() { # dup_pairs <base> <job id>
  api_get "$1" "/openapi/instance/list?job_id=$2" | python3 -c "
import re,sys,json,datetime
def parse(s):
    y,mo,d,h,mi,se,frac=re.match(r'(\d{4})-(\d\d)-(\d\d)[T ](\d\d):(\d\d):(\d\d)\.?(\d*)',s).groups()
    return datetime.datetime(int(y),int(mo),int(d),int(h),int(mi),int(se),int((frac+'000000')[:6]))
ts=sorted(parse(i['create_time']) for i in json.load(sys.stdin)['data'] or [])
print(sum(1 for a,b in zip(ts,ts[1:]) if (b-a).total_seconds()<0.5))
"
}
wait_health() {
  local i=0
  until curl -sf -m 2 "$1/healthz" -o /dev/null; do
    i=$((i+1)); [ $i -gt 60 ] && return 1; sleep 0.25
  done
  return 0
}
start_server() { # start_server <port> <logfile> <pid 变量名>
  GOJOB_CLUSTER_SECRET=$SECRET GOJOB_ADDR=:"$1" GOJOB_DB_DRIVER=sqlite GOJOB_DB_DSN=$DB \
    "$BIN" >"$2" 2>&1 &
  local pid=$!
  PIDS+=($pid)
  printf -v "$3" '%s' "$pid"
}

echo "== 0. 构建并起两个副本（共用 $DB）"
cd "$(dirname "$0")/.." || exit 1
if ! $GO build -o "$BIN" ./cmd/gojob-server; then bad "编译失败"; exit 1; fi
ok "编译 gojob-server"
# 滚动启动：两进程同时对同一套库跑建表会撞 DDL 锁（sqlite 直接 SQLITE_BUSY）。
# 真实部署也按"先滚动升级 schema，再滚副本"操作，这里保持同一口径。
start_server "$A_PORT" "$WORK/a.log" PID_A
if ! wait_health "$A"; then bad "A 没能启动"; tail -20 "$WORK/a.log"; exit 1; fi
start_server "$B_PORT" "$WORK/b.log" PID_B
if wait_health "$B"; then ok "两个副本均就绪"; else
  bad "B 没能启动"; tail -20 "$WORK/a.log" "$WORK/b.log"; exit 1
fi

echo "== 1. 令牌跨副本：A 登录换的票，B 也认"
LOGIN=$(printf '{"username":"admin","password":"admin123"}')
R=$(curl -s -m 8 -X POST "$A/openapi/user/login" -H 'Content-Type: application/json' -d "$LOGIN")
TOKEN=$(printf '%s' "$R" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] && ok "A 登录换到会话票" || { bad "登录失败：$R"; exit 1; }
ME=$(api_get "$B" /openapi/user/me)
want_code "B 校验 A 签发的会话票" 0 "$ME"

echo "== 2. 建项目与两个任务（API 型测派发，FIXED_RATE 测定时归属）"
BODY=$(printf '{"app_id":"%s","app_name":"集群验收","password":"%s"}' "$APP" "$PW")
R=$(api "$A" /openapi/app/create -H 'Content-Type: application/json' -d "$BODY")
want_code "A 上创建项目" 0 "$R"
BODY=$(printf '{"app_id":"%s","job_name":"manual","processor":"hello","time_expression_type":"API"}' "$APP")
R=$(api "$B" /openapi/job/create -H 'Content-Type: application/json' -d "$BODY")
want_code "B 上创建 API 型任务" 0 "$R"
# instance_num_limit 显式给大：默认 3，而第 5 步之前没有 worker 在消费，
# 3 条 WAITING 实例就会把后续触发全挡住，第 6 步便测不出真实触发频率。
BODY=$(printf '{"app_id":"%s","job_name":"every3s","processor":"hello","time_expression_type":"FIXED_RATE","time_expression":"3000","instance_num_limit":100}' "$APP")
R=$(api "$A" /openapi/job/create -H 'Content-Type: application/json' -d "$BODY")
want_code "A 上创建 3s 固定频率任务" 0 "$R"
# job/list 是 PageResult（data.data 才是行），page_size=0 一次拿全
JOBS=$(api_get "$A" "/openapi/job/list?app_id=$APP&page_size=0")
J_API=$(printf '%s' "$JOBS" | python3 -c "
import sys,json
d={j['job_name']:j['id'] for j in json.load(sys.stdin)['data']['data']}
print(d.get('manual'))")
J_3S=$(printf '%s' "$JOBS" | python3 -c "
import sys,json
d={j['job_name']:j['id'] for j in json.load(sys.stdin)['data']['data']}
print(d.get('every3s'))")
LIST=$(api_get "$B" /openapi/app/list)
CS=$(printf '%s' "$LIST" | python3 -c "
import sys,json
rows=[a for a in json.load(sys.stdin)['data'] if a['app_id']=='$APP']
print('no-key' if rows and 'current_server' not in rows[0] else (rows[0].get('current_server') if rows else 'no-app'))")
case "$CS" in
  no-key) bad "项目列表没有 current_server 字段" ;;
  no-app) bad "项目列表里找不到 $APP" ;;
  None|'') ok "项目列表带调度归属列（新项目暂无持有者，符合预期）" ;;
  *) ok "项目列表显示调度归属：$CS" ;;
esac

echo "== 3. worker 在 A 注册的票 B 也认；在线节点两个副本都看得见"
BODY=$(printf '{"instance_id":"probe-w1","app_id":"%s","password":"%s","address":"probe:1","sdk_type":"GO"}' "$APP" "$PW")
R=$(curl -s -m 8 -X POST "$A/worker/register" -H 'Content-Type: application/json' -d "$BODY")
WTK=$(printf '%s' "$R" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$WTK" ] && ok "A 上注册换到 worker 票" || bad "worker 注册失败：$R"
BODY=$(printf '{"instance_id":"probe-w1","app_id":"%s","address":"probe:1","token":"%s"}' "$APP" "$WTK")
R=$(wapi "$B" /worker/heartbeat "$BODY")
want_code "B 校验 A 签发的 worker 票" 0 "$R"
SEEN=$(api_get "$A" "/openapi/app/workers?app_id=$APP" \
  | python3 -c "
import sys,json
print(sum(1 for w in json.load(sys.stdin)['data'] or [] if w['instance_id']=='probe-w1'))")
if [ "$SEEN" = "1" ]; then ok "心跳打在 B 上的节点，A 的节点列表里就有（在线表落库、跨副本共享）"
else bad "A 看不到注册在 B 的节点（probe-w1 计数 $SEEN）"; fi

echo "== 4. app 级租约：只有一个副本持有该项目，且两个副本看法一致"
sleep 12
LA=$(lease_of "$A" "$APP")
LB=$(lease_of "$B" "$APP")
echo "        A 视角: ${LA:-无}    B 视角: ${LB:-无}"
HOLD=$(printf '%s' "$LA" | awk '{print $1}')
if [ "$LA" = "$LB" ]; then ok "两个副本对归属的看法一致"; else bad "归属视图不一致：$LA vs $LB"; fi
if [ -n "$HOLD" ]; then ok "有副本持有该项目（$HOLD）"; else bad "没有副本持有该项目"; fi
ALIVE=$(printf '%s' "$LA" | awk '{print $3}')
if [ "$ALIVE" = "1" ]; then ok "租约处于有效期"; else bad "租约已过期或缺行"; fi
NID_A=$(node_id_of "$A" ":$A_PORT")
NID_B=$(node_id_of "$B" ":$B_PORT")
OWNERS=$(api_get "$A" /openapi/system/nodes | python3 -c "
import sys,json
d=json.load(sys.stdin)['data']
print(sum(1 for s in d['servers'] if '$APP' in (s['owned_apps'] or [])))")
if [ "$OWNERS" = "1" ]; then ok "整个集群只有 1 个副本声明持有 $APP"
else bad "声明持有 $APP 的副本有 ${OWNERS:-?} 个"; fi
case "$HOLD" in
  "$NID_A") OWNER=$A; SUCCER=$B; OWNER_LOG=$WORK/a.log; OWNER_PID=$PID_A; OWNER_PORT=$A_PORT ;;
  "$NID_B") OWNER=$B; SUCCER=$A; OWNER_LOG=$WORK/b.log; OWNER_PID=$PID_B; OWNER_PORT=$B_PORT ;;
  *) bad "归属 ${HOLD:-空} 对不上任何一个副本（A=$NID_A B=$NID_B）" ;;
esac

echo "== 5. 跨副本唤醒：归属副本入队，连在另一副本上的 worker 多快拿到"
if [ -z "$OWNER" ]; then
  bad "跳过派发验收：第 4 步没认出归属副本"
  LAT=99
else
  # worker 只连"不是归属方"的那个副本：入队方与发任务方必然是两个进程
  GOJOB_WORKER_SERVER=$SUCCER GOJOB_WORKER_APP=$APP GOJOB_WORKER_PASSWORD=$PW \
  $GO run ./cmd/gojob-demo >"$WORK/worker.log" 2>&1 &
  PIDS+=($!)
  ready=0
  i=0
  while [ "$ready" != "1" ] && [ $i -lt 80 ]; do
    sleep 0.5
    ready=$(api_get "$OWNER" "/openapi/app/workers?app_id=$APP" \
      | python3 -c "
import sys,json
print(1 if any(w['sdk_type']=='GO' and w['address']!='probe:1' for w in json.load(sys.stdin)['data'] or []) else 0)")
    i=$((i+1))
  done
  if [ "$ready" = "1" ]; then ok "worker 已连上 $( [ "$SUCCER" = "$A" ] && echo A || echo B )（非归属副本）"
  else bad "worker 没能在 40s 内注册上（$SUCCER）"; tail -5 "$WORK/worker.log"; fi
  T0=$(date +%s)
  BODY=$(printf '{"job_id":%s,"action":"trigger"}' "$J_API")
  api "$OWNER" /openapi/job/operate -H 'Content-Type: application/json' -d "$BODY" >/dev/null
  ST=""
  i=0
  while [ $i -lt 60 ]; do
    sleep 0.25
    ST=$(latest_status "$SUCCER" "$J_API")
    case "$ST" in SUCCEED|FAILED) break;; esac
    i=$((i+1))
  done
  LAT=$(( $(date +%s) - T0 ))
  case "$ST" in
    SUCCEED) ok "归属副本入队的任务被连在另一副本的 worker 执行成功，端到端 ${LAT}s" ;;
    *) bad "任务没跑成功（status=${ST:-无}，${LAT}s）" ;;
  esac
  if [ "$LAT" -lt 5 ]; then
    ok "跨副本唤醒时延 ${LAT}s < 5s（信号 1s 一跳，没退到 5s 兜底重查）"
  else
    bad "跨副本唤醒时延 ${LAT}s：入队广播没能传到另一个副本"
  fi
fi

echo "== 6. 定时任务只有一个副本在触发：12s 内约 4 次，不是双跑的 8 次"
N0=$(inst_count "$A" "$J_3S")
sleep 12
N1=$(inst_count "$A" "$J_3S")
GROW=$((N1-N0))
if [ $GROW -ge 3 ] && [ $GROW -le 5 ]; then ok "12s 新增 $GROW 个实例（单副本调度）"
else bad "12s 新增 $GROW 个实例，期望 3~5（双跑会接近 8）"; fi
DUP=$(dup_pairs "$A" "$J_3S")
if [ "$DUP" = "0" ]; then ok "没有一对实例的创建时间相差 500ms 以内"
else bad "$DUP 对实例几乎同时创建（重复触发）"; fi

echo "== 7. 冻结归属副本（模拟分区里的旧主）→ 另一副本接管"
EP0=$(lease_of "$SUCCER" "$APP" | awk '{print $2}')
kill -STOP "$OWNER_PID"
sleep 20
L2=$(lease_of "$SUCCER" "$APP")
echo "        接管后: ${L2:-无}"
HOLD2=$(printf '%s' "$L2" | awk '{print $1}')
ALIVE2=$(printf '%s' "$L2" | awk '{print $3}')
if [ "$ALIVE2" = "1" ]; then ok "冻结后依然有有效租约持有者"; else bad "冻结后无人持有该项目"; fi
if [ -n "$HOLD2" ] && [ "$HOLD2" != "$HOLD" ]; then ok "归属已切到存活副本（$HOLD → $HOLD2）"
else bad "归属没接管，仍写着被冻结的 $HOLD"; fi
EP1=$(printf '%s' "$L2" | awk '{print $2}')
if [ "${EP1:-0}" -gt "${EP0:-0}" ]; then ok "接管把 epoch 从 $EP0 推到 $EP1（epoch 只在接管时增长）"
else bad "epoch 未推进（$EP0 → $EP1），接管与续约无法区分"; fi
M0=$(inst_count "$SUCCER" "$J_3S")
sleep 9
M1=$(inst_count "$SUCCER" "$J_3S")
if [ $((M1-M0)) -ge 2 ]; then ok "接管方继续调度（9s 新增 $((M1-M0)) 个实例）"
else bad "接管后调度停了（9s 新增 $((M1-M0))）"; fi

echo "== 8. 解冻旧主：它只能交出归属，写不进库"
kill -CONT "$OWNER_PID"
sleep 8
HOLD3=$(lease_of "$SUCCER" "$APP" | awk '{print $1}')
if [ "$HOLD3" = "$HOLD2" ]; then ok "旧主复活后没有抢回归属"; else bad "旧主复活即篡位：$HOLD3"; fi
if grep -q "lost app $APP" "$OWNER_LOG"; then ok "旧主日志里看得到自己失去归属（续约失败/fencing）"
else bad "旧主日志里没有失去归属的记录"; fi
K0=$(inst_count "$SUCCER" "$J_3S")
sleep 9
K1=$(inst_count "$SUCCER" "$J_3S")
D=$((K1-K0))
if [ $D -le 5 ]; then ok "解冻后 9s 仍只新增 $D 个实例，两个副本没有双跑"
else bad "解冻后 9s 新增 $D 个实例，疑似僵尸主参与调度"; fi
DUP2=$(dup_pairs "$SUCCER" "$J_3S")
if [ "$DUP2" = "0" ]; then ok "全程没有任何一次重复触发（500ms 内双份实例为 0）"
else bad "解冻后出现 $DUP2 对几乎同时创建的实例"; fi

echo
echo "结果：PASS=$pass FAIL=$fail"
[ $fail -eq 0 ]
