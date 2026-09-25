#!/usr/bin/env bash
# owner 路由（C 方案）实机验收：两个副本 + 十个 worker，回答三个问题：
#   ① worker 按什么条件选择并确认 owner？（初始分散 → 收敛到持有租约的那台）
#   ② owner 被冻结、租约易主之后，worker 怎么跟到新 owner？跟得上吗？
#   ③ owner 提示是错的/连不上时，任务还跑得动吗？（"提示不承重"这条主张的证据）
#
# 本机没有 docker 与 mysql/pgsql，所以用 sqlite：WAL + _txlock=immediate + busy_timeout
# 是真跨进程锁，与 mysql 条件 UPDATE 同口径。
#
# 注意 JSON 一律用 printf 先拼进变量：macOS 自带 bash 3.2 在"双引号里嵌套 $( ) 再套转义引号"
# 时解析会错乱（实测把一条 curl 拆成三条）。
# 用法: scripts/verify-owner-routing.sh   （可用 GO/NW/A_PORT/B_PORT 覆盖）
set -u
GO=${GO:-go}
A_PORT=${A_PORT:-7851}
B_PORT=${B_PORT:-7852}
C_PORT=${C_PORT:-7853}
D_PORT=${D_PORT:-7854}
NW=${NW:-10}
APP=${APP:-route_app}
PW=${PW:-route123}
OFF_APP=${OFF_APP:-off_app}
SECRET=${SECRET:-route-verify-secret}
DEAD=http://127.0.0.1:59999   # 永远连不上的"对外地址"，模拟 advertise 配错
A=http://127.0.0.1:$A_PORT
B=http://127.0.0.1:$B_PORT
C=http://127.0.0.1:$C_PORT
D=http://127.0.0.1:$D_PORT
WORK=$(mktemp -d)
DB=$WORK/route.db
DB2=$WORK/off.db
BIN=$WORK/gojob-server
WBIN=$WORK/gojob-demo
pass=0 fail=0
PIDS=()
WLOG=()
TOKEN=

ok()  { pass=$((pass+1)); echo "  PASS  $1"; }
bad() { fail=$((fail+1)); echo "  FAIL  $1"; }

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
api() { # api <base> <path> [curl 参数...]
  local base=$1 path=$2
  shift 2
  curl -s -m 8 -X POST "$base$path" -H "X-GoJob-Token: $TOKEN" "$@"
}
api_get() { curl -s -m 8 "$1$2" -H "X-GoJob-Token: $TOKEN"; }
login() { # login <base> —— 换一张该套库自己的会话票（两套库不共享吊销基线，各登各的）
  local body='{"username":"admin","password":"admin123"}'
  TOKEN=$(curl -s -m 8 -X POST "$1/openapi/user/login" -H 'Content-Type: application/json' -d "$body" \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  [ -n "$TOKEN" ] && ok "$1 登录换到会话票" || bad "$1 登录失败"
}

wait_health() {
  local i=0
  until curl -sf -m 2 "$1/healthz" -o /dev/null; do
    i=$((i+1)); [ $i -gt 80 ] && return 1; sleep 0.25
  done
  return 0
}
# start_server <port> <public_url> <db> <log>  —— public_url 就是下发给 worker 的"可达地址"
start_server() {
  GOJOB_CLUSTER_SECRET=$SECRET GOJOB_ADDR=:"$1" GOJOB_PUBLIC_URL=$2 \
    GOJOB_DB_DRIVER=sqlite GOJOB_DB_DSN=$3 "$BIN" >"$4" 2>&1 &
  PIDS+=($!)
}
listen_pid() { # 该端口上的监听进程 pid
  lsof -nP -t -i tcp:"$1" -sTCP:LISTEN 2>/dev/null | head -1
}
# owner_url <base> <app>：租约持有者的对外地址（= worker 应该收敛过去的那台）
owner_url() {
  api_get "$1" /openapi/system/nodes | python3 -c "
import sys,json
d=json.load(sys.stdin)['data']
url={s['node_id']:s['public_url'] for s in d['servers'] or []}
for l in d['leases'] or []:
    if l['app_id']=='$2' and l['alive']: print(url.get(l['holder'],'?')); break
"
}
# poll_of <base> <public_url>：该副本当前挂了多少条 worker 长轮询（5s 一次的快照值）
poll_of() {
  api_get "$1" /openapi/system/nodes | python3 -c "
import sys,json
for s in json.load(sys.stdin)['data']['servers'] or []:
    if s['public_url']=='$2': print(s['poll_inflight']); break
"
}
epoch_of() {
  api_get "$1" /openapi/system/nodes | python3 -c "
import sys,json
for l in json.load(sys.stdin)['data']['leases'] or []:
    if l['app_id']=='$2': print(l['epoch']); break
"
}
inst_count() {
  api_get "$1" "/openapi/instance/list?job_id=$2" \
    | python3 -c "import sys,json;print(len(json.load(sys.stdin)['data'] or []))"
}
succeed_count() {
  api_get "$1" "/openapi/instance/list?job_id=$2" \
    | python3 -c "
import sys,json
print(sum(1 for i in json.load(sys.stdin)['data'] or [] if i['status']=='SUCCEED'))"
}
dup_pairs() { # 相邻实例创建间隔 <0.5s 的对数：3s 周期下出现即为双跑
  api_get "$1" "/openapi/instance/list?job_id=$2" | python3 -c "
import re,sys,json,datetime
def parse(s):
    y,mo,d,h,mi,se,frac=re.match(r'(\d{4})-(\d\d)-(\d\d)[T ](\d\d):(\d\d):(\d\d)\.?(\d*)',s).groups()
    return datetime.datetime(int(y),int(mo),int(d),int(h),int(mi),int(se),int((frac+'000000')[:6]))
ts=sorted(parse(i['create_time']) for i in json.load(sys.stdin)['data'] or [])
print(sum(1 for a,b in zip(ts,ts[1:]) if (b-a).total_seconds()<0.5))
"
}
# start_worker <序号> <app> <口令> <server 列表> —— drain 端口按序号错开，日志各自一个文件
start_worker() {
  local i=$1 app=$2 pw=$3 servers=$4
  local log=$WORK/w$i.log
  GOJOB_WORKER_APP=$app GOJOB_WORKER_PASSWORD=$pw GOJOB_WORKER_SERVER=$servers \
  GOJOB_WORKER_DRAIN_ADDR=:$((7870+i)) "$WBIN" >"$log" 2>&1 &
  PIDS+=($!)
  WLOG[$i]=$log
}
migrations() { # 该 worker 一共迁移过几次长轮询
  local n
  n=$(grep -c '长轮询迁过去' "$1" 2>/dev/null)
  printf '%s' "${n:-0}"
}

echo "== 0. 构建"
cd "$(dirname "$0")/.." || exit 1
if ! $GO build -o "$BIN" ./cmd/gojob-server; then bad "编译 server 失败"; exit 1; fi
if ! $GO build -o "$WBIN" ./cmd/gojob-demo; then bad "编译 demo worker 失败"; exit 1; fi
ok "编译 gojob-server / gojob-demo"

echo
echo "== 1. 两个副本 + $NW 个 worker：worker 如何选择并确认 owner"
# 滚动启动：两进程同时对同一套库建表会撞 DDL 锁
start_server "$A_PORT" "$A" "$DB" "$WORK/a.log"
wait_health "$A" || { bad "A 没能启动"; tail -20 "$WORK/a.log"; exit 1; }
start_server "$B_PORT" "$B" "$DB" "$WORK/b.log"
wait_health "$B" || { bad "B 没能启动"; tail -20 "$WORK/b.log"; exit 1; }
ok "A(:$A_PORT) 与 B(:$B_PORT) 就绪，各自上报对外地址"
login "$A"

BODY=$(printf '{"app_id":"%s","app_name":"owner 路由验收","password":"%s"}' "$APP" "$PW")
want_code "创建项目" 0 "$(api "$A" /openapi/app/create -H 'Content-Type: application/json' -d "$BODY")"
# instance_num_limit 显式给大（默认 3），否则 3 条 WAITING 就把后续触发全挡住了
BODY=$(printf '{"app_id":"%s","job_name":"every3s","processor":"hello","time_expression_type":"FIXED_RATE","time_expression":"3000","instance_num_limit":100}' "$APP")
want_code "创建 3s 固定频率任务" 0 "$(api "$B" /openapi/job/create -H 'Content-Type: application/json' -d "$BODY")"
J=$(api_get "$A" "/openapi/job/list?app_id=$APP&page_size=0" | python3 -c "
import sys,json
d={x['job_name']:x['id'] for x in json.load(sys.stdin)['data']['data']}
print(d.get('every3s'))")

sleep 8
OWNER=$(owner_url "$A" "$APP")
PEER=$A
[ "$OWNER" = "$A" ] && PEER=$B
if [ -z "$OWNER" ] || [ "$OWNER" = "?" ]; then
  bad "查不到 owner 地址（租约或 server_nodes 没落库）"; exit 1
fi
echo "        租约持有者 = $OWNER    对端 = $PEER"
ok "owner 可由租约 + 副本对外地址推出"

# 一半 worker 先连 A、一半先连 B：制造初始 5/5 分散，收敛必须靠 owner 提示完成
i=0
while [ $i -lt $NW ]; do
  if [ $((i % 2)) -eq 0 ]; then LIST="$A,$B"; else LIST="$B,$A"; fi
  start_worker $i "$APP" "$PW" "$LIST"
  i=$((i+1))
done
ok "$NW 个 worker 已启动（地址列表一半 A 优先、一半 B 优先）"

# 收敛要等两个东西：①连续 2 跳心跳确认（10s）②迁移前那条挂着的长轮询自然结束
# （首地址非 owner 的 worker，它的 fetch 会一直挂在旧副本直到 MaxWaitSeconds=30s 到点，
#  我们不打断在途请求，所以"首选地址"立刻生效、"长轮询占用"至多再等一个周期）。
sleep 48
PO=$(poll_of "$A" "$OWNER")
PP=$(poll_of "$A" "$PEER")
echo "        收敛后长轮询分布：owner=$OWNER ${PO:-?} 条 / 对端 $PEER ${PP:-?} 条"
if [ -n "$PO" ] && [ "$PO" -ge $((NW-1)) ]; then ok "worker 已收敛到 owner（$PO/$NW 挂在 owner 上）"
else bad "worker 没收敛到 owner（owner 上只有 ${PO:-0} 条长轮询）"; fi
if [ -n "$PP" ] && [ "$PP" -le 1 ]; then ok "对端副本基本空挂（$PP 条，只剩迁移途中的）"
else bad "对端还挂着 $PP 条长轮询，首选地址没迁走"; fi

# 选择条件：首地址本来就是 owner 的那一半不该迁移；另一半必须恰好迁一次
MIG=0; STAY=0; WRONG=0
i=0
while [ $i -lt $NW ]; do
  n=$(migrations "${WLOG[$i]}")
  first=$A; [ $((i % 2)) -eq 1 ] && first=$B
  if [ "$first" = "$OWNER" ]; then
    STAY=$((STAY+1))
    [ "$n" != "0" ] && { WRONG=$((WRONG+1)); echo "        worker#$i 首地址就是 owner，却迁移了 $n 次"; }
  else
    MIG=$((MIG+1))
    [ "$n" = "0" ] && { WRONG=$((WRONG+1)); echo "        worker#$i 首地址非 owner，却没迁移"; }
    [ "$n" -gt 1 ] && { WRONG=$((WRONG+1)); echo "        worker#$i 迁移了 $n 次，防抖没生效（来回搬家）"; }
  fi
  i=$((i+1))
done
if [ "$WRONG" = "0" ]; then
  ok "选择条件成立：$STAY 个首地址即 owner 的原地不动，$MIG 个非 owner 的恰好各迁 1 次"
else
  bad "$WRONG 个 worker 的迁移次数不符合「连续 2 跳确认才迁」的预期"
fi
echo "        —— worker#1（首地址 $B）的确认过程原文："
grep -E '确认中|learned|长轮询迁过去' "${WLOG[1]}" | head -4 | sed 's/^/          /'

# 频率只在一个固定窗口里判定：从任务创建到这里已经跑了约 40s，累计数不能直接换算周期
N1=$(inst_count "$A" "$J")
sleep 12
N1B=$(inst_count "$A" "$J")
GAIN1=$((N1B-N1))
if [ "$GAIN1" -ge 3 ] && [ "$GAIN1" -le 5 ]; then ok "3s 任务在 $NW 个 worker 下按周期出实例（12s 窗口新增 $GAIN1 个）"
else bad "12s 窗口新增 $GAIN1 个实例，3s 周期应落在 3~5"; fi
if [ "$(dup_pairs "$A" "$J")" = "0" ]; then ok "无重复触发（相邻实例创建间隔都 >=0.5s）"
else bad "出现相邻 <0.5s 的实例对，双跑了"; fi

echo
echo "== 2. 冻结 owner（SIGSTOP，模拟长 GC / 主机挂起）：租约易主后 worker 跟不跟得上"
OWNER_PORT=${OWNER##*:}; PEER_PORT=${PEER##*:}
OWNER_PID=$(listen_pid "$OWNER_PORT")
if [ -z "$OWNER_PID" ]; then bad "找不到 owner 进程，后续步骤无法进行"; exit 1; fi
EP0=$(epoch_of "$PEER" "$APP")
kill -STOP "$OWNER_PID"
# 冻结后 owner 不再应答 HTTP，所有查询都走活着的那台（读的是同一套库）
Q=$PEER
echo "        已冻结 $OWNER (pid $OWNER_PID)；接管窗口 ≈ TTL 10s + 扫描 3s + 确认 2 跳 10s + 采样 5s"
N0=$N1B
# 窗口 = 接管（TTL 10s + 扫描）+ 心跳超时认出僵尸副本（15s）+ 2 跳确认（10s）
#        + 挂着的长轮询到点（fetch 预算 wait+15s = 45s，与前面几项并行发生）
sleep 50
OWNER2=$(owner_url "$Q" "$APP")
EP1=$(epoch_of "$Q" "$APP")
if [ "$OWNER2" = "$PEER" ]; then ok "租约已易主到对端（$OWNER -> $OWNER2），epoch $EP0 -> $EP1"
else bad "接管失败：owner 仍是 ${OWNER2:-无}"; fi
if [ "$EP1" -gt "$EP0" ]; then ok "接管递增了 fencing epoch"; else bad "epoch 没变（$EP0 -> $EP1）"; fi

PO2=$(poll_of "$Q" "$OWNER2")
echo "        迁移后长轮询分布：新 owner $OWNER2 ${PO2:-?} 条"
if [ -n "$PO2" ] && [ "$PO2" -ge $((NW-1)) ]; then ok "worker 跟着租约搬到了新 owner（$PO2/$NW）"
else bad "worker 没跟着迁移（新 owner 上只有 ${PO2:-0} 条）"; fi

N2=$(inst_count "$Q" "$J")
GAIN=$((N2-N0))
if [ "$GAIN" -ge 6 ]; then ok "冻结期间调度没停（50s 内新增 $GAIN 个实例，新主在续跑）"
else bad "冻结期间只新增 $GAIN 个实例（50s 窗口、接管后应 >=6），接管有断档"; fi
if [ "$(succeed_count "$Q" "$J")" -ge 3 ]; then ok "冻结期间产生的实例仍被执行成功（回报打到了活着的副本）"
else bad "SUCCEED 实例太少，跨副本下发/回报链路疑似断了"; fi

kill -CONT "$OWNER_PID"
sleep 14
OWNER3=$(owner_url "$Q" "$APP")
if [ "$OWNER3" = "$OWNER2" ]; then ok "旧主解冻后不抢回调度权（仍是 $OWNER3，避免来回抖动）"
else bad "旧主解冻后把租约抢回去了：$OWNER2 -> $OWNER3"; fi
if [ "$(dup_pairs "$Q" "$J")" = "0" ]; then ok "整个冻结/解冻周期内仍无双跑"
else bad "冻结/解冻过程中出现双跑"; fi
echo "        —— 一次接管期间 worker#0 的路由日志："
grep -E 'unreachable|本轮超时|确认中|长轮询迁过去|abandoning' "${WLOG[0]}" | tail -6 | sed 's/^/          /'

echo
echo "== 3. owner 的对外地址是错的（连不上）：提示必须不承重"
# 另起一套库。先起 C（对外地址配成永远连不上的 DEAD），再起 D。
# C 启动时这套库里只有它一个副本，所以 off_app 的租约必然归它，
# 而它对外的地址是坏的 —— 这就是 advertise 配错的现场。
start_server "$C_PORT" "$DEAD" "$DB2" "$WORK/c.log"
wait_health "$C" || { bad "C 没能启动"; tail -20 "$WORK/c.log"; exit 1; }
login "$C"
BODY=$(printf '{"app_id":"%s","app_name":"坏地址副本","password":"%s"}' "$OFF_APP" "$PW")
want_code "C 上创建项目" 0 "$(api "$C" /openapi/app/create -H 'Content-Type: application/json' -d "$BODY")"
BODY=$(printf '{"app_id":"%s","job_name":"every3s","processor":"hello","time_expression_type":"FIXED_RATE","time_expression":"3000","instance_num_limit":100}' "$OFF_APP")
want_code "C 上创建 3s 任务" 0 "$(api "$C" /openapi/job/create -H 'Content-Type: application/json' -d "$BODY")"
J2=$(api_get "$C" "/openapi/job/list?app_id=$OFF_APP&page_size=0" | python3 -c "
import sys,json
d={x['job_name']:x['id'] for x in json.load(sys.stdin)['data']['data']}
print(d.get('every3s'))")
sleep 8
start_server "$D_PORT" "$D" "$DB2" "$WORK/d.log"
wait_health "$D" || { bad "D 没能启动"; tail -20 "$WORK/d.log"; exit 1; }
login "$D"
sleep 10
BAD_OWNER=$(owner_url "$D" "$OFF_APP")
if [ "$BAD_OWNER" = "$DEAD" ]; then ok "D 如实下发 owner=$DEAD（提示是错的：那是个连不上的地址）"
else bad "预期 owner 是坏地址 $DEAD，实得 ${BAD_OWNER:-无}"; fi

start_worker 20 "$OFF_APP" "$PW" "$D"
W20=$WORK/w20.log
sleep 26
if grep -q 'unreachable' "$W20"; then ok "worker 试过这个 owner，发现连不上（日志即证据）"
else bad "worker 没试过坏 owner，跳转没发生"; fi
if grep -q "fail over to $D" "$W20"; then ok "连不上就退回已知副本 $D，继续用它收发"
else bad "没退回可用副本"; fi
N3=$(inst_count "$D" "$J2")
S3=$(succeed_count "$D" "$J2")
if [ "$N3" -ge 4 ] && [ "$S3" -ge 4 ]; then
  ok "任务照常：$N3 个实例、$S3 个执行成功（实例由连不上的 C 触发，经 D 下发给 worker）"
else bad "坏提示把任务打断了（实例 $N3 / 成功 $S3）"; fi
if [ -n "$(listen_pid 7890)" ]; then ok "worker 进程健在（没因为提示错误退出）"; else bad "worker 退出了"; fi
if [ "$(dup_pairs "$D" "$J2")" = "0" ]; then ok "坏地址场景下也没有双跑"; else bad "坏地址场景出现双跑"; fi
FLIP=$(migrations "$W20")
REJ=$(grep -c 'fail over to' "$W20" 2>/dev/null); REJ=${REJ:-0}
echo "        代价观测：26s 内该 worker 对坏 owner 发起 $FLIP 次跳转、$REJ 次故障转移（可观测的空转，不影响正确性）"

echo
echo "== 结果：PASS $pass / FAIL $fail"
[ "$fail" = "0" ] || exit 1
