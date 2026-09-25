#!/usr/bin/env bash
# Java SDK 的 owner 路由实机验收：两个副本 + 4 个 Java worker（QuickStart，地址列表两两反向），
# 验证 Go SDK 那套规则在 Java 侧行为一致：
#   ① 初始分散 → 靠心跳下发的 cluster_hint 收敛到租约持有者（连续 2 跳确认才迁）
#   ② 冻结 owner → 短超时认出僵尸副本、退回另一台、跟到新 owner，任务照常执行
#   ③ 全程不依赖 LB/VIP
#
# 前提：本机有 JDK（~/.sdkman）与 Maven wrapper 发行版；mvn 用离线模式，依赖已在本地仓库。
# 用法: scripts/verify-java-routing.sh
set -u
GO=${GO:-go}
A_PORT=${A_PORT:-7861}
B_PORT=${B_PORT:-7862}
NW=${NW:-4}
APP=${APP:-jroute_app}
PW=${PW:-jroute123}
SECRET=${SECRET:-jroute-verify-secret}
A=http://127.0.0.1:$A_PORT
B=http://127.0.0.1:$B_PORT
JAVA_HOME=${JAVA_HOME:-$HOME/.sdkman/candidates/java/current}
MVN=${MVN:-$(find "$HOME/.m2/wrapper/dists" -name mvn -type f 2>/dev/null | head -1)}
WORK=$(mktemp -d)
DB=$WORK/jroute.db
BIN=$WORK/gojob-server
CP_FILE=$WORK/cp.txt
pass=0 fail=0
PIDS=()
JLOG=()
TOKEN=

export JAVA_HOME
ok()  { pass=$((pass+1)); echo "  PASS  $1"; }
bad() { fail=$((fail+1)); echo "  FAIL  $1"; }
cleanup() {
  for p in ${PIDS[@]+"${PIDS[@]}"}; do kill -9 "$p" 2>/dev/null; done
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

jget() {
  printf '%s' "$1" | python3 -c "
import sys,json
try: root=json.load(sys.stdin)
except Exception as e: print('parse-error',e); sys.exit()
try: print($2)
except Exception as e: print('missing',e)
"
}
api() { curl -s -m 8 -X POST "$1$2" -H "X-GoJob-Token: $TOKEN" "${@:3}"; }
api_get() { curl -s -m 8 "$1$2" -H "X-GoJob-Token: $TOKEN"; }
wait_health() {
  local i=0
  until curl -sf -m 2 "$1/healthz" -o /dev/null; do
    i=$((i+1)); [ $i -gt 80 ] && return 1; sleep 0.25
  done
}
start_server() {
  GOJOB_CLUSTER_SECRET=$SECRET GOJOB_ADDR=:"$1" GOJOB_PUBLIC_URL=$2 \
    GOJOB_DB_DRIVER=sqlite GOJOB_DB_DSN=$3 "$BIN" >"$4" 2>&1 &
  PIDS+=($!)
}
owner_url() {
  api_get "$1" /openapi/system/nodes | python3 -c "
import sys,json
d=json.load(sys.stdin)['data']
url={s['node_id']:s['public_url'] for s in d['servers'] or []}
for l in d['leases'] or []:
    if l['app_id']=='$2' and l['alive']: print(url.get(l['holder'],'?')); break
"
}
poll_of() {
  api_get "$1" /openapi/system/nodes | python3 -c "
import sys,json
for s in json.load(sys.stdin)['data']['servers'] or []:
    if s['public_url']=='$2': print(s['poll_inflight']); break
"
}
# /openapi/app/workers 走 registry.Workers(appId)，本身只含"分发候选集里的活节点"（没有 alive 字段），
# 所以这里只按 sdk_type 过滤，别再去判一个不存在的字段。
online_workers() {
  api_get "$1" "/openapi/app/workers?app_id=$APP" | python3 -c "
import sys,json
print(sum(1 for w in json.load(sys.stdin)['data'] or [] if str(w.get('sdk_type','')).startswith('JAVA')))"
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
dup_pairs() {
  api_get "$1" "/openapi/instance/list?job_id=$2" | python3 -c "
import re,sys,json,datetime
def parse(s):
    y,mo,d,h,mi,se,frac=re.match(r'(\d{4})-(\d\d)-(\d\d)[T ](\d\d):(\d\d):(\d\d)\.?(\d*)',s).groups()
    return datetime.datetime(int(y),int(mo),int(d),int(h),int(mi),int(se),int((frac+'000000')[:6]))
ts=sorted(parse(i['create_time']) for i in json.load(sys.stdin)['data'] or [])
print(sum(1 for a,b in zip(ts,ts[1:]) if (b-a).total_seconds()<0.5))
"
}
# start_java_worker <序号> <server 列表> —— 用 test 源码里的 QuickStart，日志各自一个文件
start_java_worker() {
  local i=$1 servers=$2
  local log=$WORK/j$i.log
  GOJOB_WORKER_SERVER=$servers GOJOB_WORKER_APP=$APP GOJOB_WORKER_PASSWORD=$PW \
    "$JAVA_HOME/bin/java" -cp "$CP" com.gojob.worker.examples.QuickStart >"$log" 2>&1 &
  PIDS+=($!)
  JLOG[$i]=$log
}

echo "== 0. 构建 Go server 与 Java core（含 test 目录里的 QuickStart）"
cd "$(dirname "$0")/.." || exit 1
if ! $GO build -o "$BIN" ./cmd/gojob-server; then bad "编译 gojob-server 失败"; exit 1; fi
if [ -z "$MVN" ] || [ ! -x "$MVN" ]; then bad "找不到 maven，跳过 Java 验收"; exit 1; fi
if ! (cd sdk-java && "$MVN" -o -q -DskipTests test-compile) >"$WORK/mvn.log" 2>&1; then
  bad "mvn test-compile 失败"; tail -20 "$WORK/mvn.log"; exit 1
fi
if ! (cd sdk-java && "$MVN" -o -q -pl gojob-worker-core dependency:build-classpath \
      -Dmdep.outputFile="$CP_FILE" -Dmdep.includeScope=test) >>"$WORK/mvn.log" 2>&1; then
  bad "取 classpath 失败"; tail -20 "$WORK/mvn.log"; exit 1
fi
CP="$PWD/sdk-java/gojob-worker-core/target/classes:$PWD/sdk-java/gojob-worker-core/target/test-classes:$(cat "$CP_FILE")"
ok "编译完成（gojob-server + gojob-worker-core/test-classes）"

echo
echo "== 1. 两个副本 + $NW 个 Java worker：收敛到 owner"
start_server "$A_PORT" "$A" "$DB" "$WORK/a.log"
wait_health "$A" || { bad "A 没能启动"; tail -20 "$WORK/a.log"; exit 1; }
start_server "$B_PORT" "$B" "$DB" "$WORK/b.log"
wait_health "$B" || { bad "B 没能启动"; tail -20 "$WORK/b.log"; exit 1; }
LOGIN=$(printf '{"username":"admin","password":"admin123"}')
TOKEN=$(curl -s -m 8 -X POST "$A/openapi/user/login" -H 'Content-Type: application/json' -d "$LOGIN" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] && ok "登录换到会话票" || { bad "登录失败"; exit 1; }
BODY=$(printf '{"app_id":"%s","app_name":"Java owner 验收","password":"%s"}' "$APP" "$PW")
want=$(jget "$(api "$A" /openapi/app/create -H 'Content-Type: application/json' -d "$BODY")" "root['code']")
[ "$want" = "0" ] && ok "创建项目" || bad "创建项目失败 code=$want"
# processor 用 QuickStart 里注册的 "hello"
BODY=$(printf '{"app_id":"%s","job_name":"every3s","processor":"hello","time_expression_type":"FIXED_RATE","time_expression":"3000","instance_num_limit":100}' "$APP")
want=$(jget "$(api "$B" /openapi/job/create -H 'Content-Type: application/json' -d "$BODY")" "root['code']")
[ "$want" = "0" ] && ok "创建 3s 任务" || bad "创建任务失败 code=$want"
J=$(api_get "$A" "/openapi/job/list?app_id=$APP&page_size=0" | python3 -c "
import sys,json
d={x['job_name']:x['id'] for x in json.load(sys.stdin)['data']['data']}
print(d.get('every3s'))")

sleep 8
OWNER=$(owner_url "$A" "$APP")
PEER=$A; [ "$OWNER" = "$A" ] && PEER=$B
echo "        租约持有者 = $OWNER    对端 = $PEER"
i=0
while [ $i -lt $NW ]; do
  if [ $((i % 2)) -eq 0 ]; then start_java_worker $i "$A,$B"; else start_java_worker $i "$B,$A"; fi
  i=$((i+1))
done
ok "$NW 个 Java worker 已启动（地址列表两两反向）"

# 等注册 + 2 跳确认 + 挂在旧地址上的长轮询让位
sleep 40
WC=$(online_workers "$A")
if [ "$WC" -ge "$NW" ]; then ok "服务端在线表看到 $WC 个 JAVA 节点（跨副本共享）"; else bad "在线 JAVA 节点只有 $WC"; fi
PO=$(poll_of "$A" "$OWNER"); PP=$(poll_of "$A" "$PEER")
echo "        收敛后长轮询分布：owner=${PO:-?} 条 / 对端=${PP:-?} 条（另有 Go 侧没有 worker，全在这 $NW 个 Java 节点上）"
if [ -n "$PO" ] && [ "$PO" -ge $NW ]; then ok "Java worker 收敛到 owner（$PO/$NW）"
else bad "Java worker 没收敛（owner 上只有 ${PO:-0} 条）"; fi
MIGOK=0; STAYOK=0; WRONG=0
i=0
while [ $i -lt $NW ]; do
  n=$(grep -c '长轮询迁过去' "${JLOG[$i]}" 2>/dev/null); n=${n:-0}
  first=$A; [ $((i % 2)) -eq 1 ] && first=$B
  if [ "$first" = "$OWNER" ]; then
    STAYOK=$((STAYOK+1)); [ "$n" != "0" ] && { WRONG=$((WRONG+1)); echo "        worker#$i 首地址即 owner 却迁了 $n 次"; }
  else
    MIGOK=$((MIGOK+1))
    [ "$n" = "0" ] && { WRONG=$((WRONG+1)); echo "        worker#$i 该迁没迁"; }
    [ "$n" -gt 1 ] && { WRONG=$((WRONG+1)); echo "        worker#$i 迁了 $n 次（防抖失效）"; }
  fi
  i=$((i+1))
done
if [ "$WRONG" = "0" ]; then ok "选择条件与 Go 一致：$STAYOK 个原地不动、$MIGOK 个各迁 1 次"
else bad "$WRONG 个 Java worker 的迁移次数不对"; fi
echo "        —— worker#1 的确认过程原文："
grep -E '确认中|学到|长轮询迁过去|servers=' "${JLOG[1]}" | head -4 | sed 's/^/          /'
if grep -q 'no server address configured' "${JLOG[0]}" 2>/dev/null; then bad "地址列表没生效"; else ok "多地址配置被正常接受（serverAddress 逗号分隔）"; fi

N1=$(inst_count "$A" "$J"); sleep 12; N2=$(inst_count "$A" "$J"); G=$((N2-N1))
if [ "$G" -ge 3 ] && [ "$G" -le 5 ]; then ok "Java worker 正常领任务并执行（12s 窗口新增 $G 个实例，3s 周期）"
else bad "12s 窗口新增 $G 个实例，不符合 3s 周期"; fi
if [ "$(succeed_count "$A" "$J")" -ge 3 ]; then ok "Java 处理器回报 SUCCEED（QuickStart 的 hello）"; else bad "没有成功实例"; fi
if [ "$(dup_pairs "$A" "$J")" = "0" ]; then ok "无重复触发"; else bad "出现双跑"; fi

echo
echo "== 2. 冻结 owner：Java worker 用短超时认出僵尸副本并跟到新 owner"
OPORT=${OWNER##*:}; PPORT=${PEER##*:}
OWNER_PID=$(lsof -nP -t -i tcp:"$OPORT" -sTCP:LISTEN 2>/dev/null | head -1)
[ -n "$OWNER_PID" ] || { bad "找不到 owner 进程"; exit 1; }
kill -STOP "$OWNER_PID"
Q=$PEER
echo "        已冻结 $OWNER (pid $OWNER_PID)"
N0=$N2
sleep 45
OWNER2=$(owner_url "$Q" "$APP")
PO2=$(poll_of "$Q" "$OWNER2")
if [ "$OWNER2" = "$PEER" ]; then ok "租约已易主（$OWNER -> $OWNER2）"; else bad "接管失败：${OWNER2:-无}"; fi
if [ -n "$PO2" ] && [ "$PO2" -ge $NW ]; then ok "Java worker 跟着搬到新 owner（$PO2/$NW）"
else bad "Java worker 没迁移（新 owner 上 ${PO2:-0} 条）"; fi
if grep -q '本轮超时\|连不上' "${JLOG[0]}" 2>/dev/null; then ok "日志里有「认出僵尸副本并换台」的记录（短超时生效）"
else bad "没有故障转移记录，说明短超时没起作用"; fi
N3=$(inst_count "$Q" "$J"); G2=$((N3-N0))
if [ "$G2" -ge 6 ]; then ok "冻结的 45s 内仍新增 $G2 个实例，调度与执行都没停"
else bad "冻结期间只新增 $G2 个实例，接管有断档"; fi
if [ "$G2" -gt 0 ] && [ "$(succeed_count "$Q" "$J")" -ge 4 ]; then ok "冻结期间的实例照样被 Java worker 执行成功（跨副本下发/回报通）"
else bad "冻结期间没有任务完成"; fi
kill -CONT "$OWNER_PID"
sleep 14
if [ "$(owner_url "$Q" "$APP")" = "$OWNER2" ]; then ok "旧主解冻后不抢回调度权"; else bad "旧主抢回了租约"; fi
if [ "$(dup_pairs "$Q" "$J")" = "0" ]; then ok "整个冻结/解冻周期无双跑"; else bad "冻结/解冻期间双跑"; fi
echo "        —— 接管期间 worker#0 的路由日志："
grep -E '连不上|本轮超时|确认中|长轮询迁过去|放弃挂' "${JLOG[0]}" | tail -6 | sed 's/^/          /'

echo
echo "== 结果：PASS $pass / FAIL $fail"
[ "$fail" = "0" ] || exit 1
