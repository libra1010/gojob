#!/usr/bin/env bash
# 容量实机验收：200 个 worker 节点在线 + 一次广播 200 份任务，毫秒级派发与收尾。
#
# 要回答的是两个具体问题，不给纸面换算：
#   A. 200 个节点只是"挂着"（各自长轮询 + 心跳）时，Server 的连接数、协程数、CPU、堆是多少；
#   B. 一次性把 200 个任务打进去（BROADCAST，每台一份），从"触发返回"到"每个节点开跑"的
#      派发延迟分位数是多少、200 份全部跑完要多久。
#
# 节点是真 worker 节点（走 SDK 的注册/换票/长轮询/回报全路径），只是处理器什么都不做。
# 一台机器起 200 个 goroutine 能压出"200 个节点在线"这件事，但**不等于**生产上是单机跑
# 200 台——生产上的瓶颈通常是那 200 台各自的算力，这里量的只有 Server 侧与派发链路。
#
# 用法: scripts/verify-capacity.sh        （NODES=200 ROUNDS=5 PORT=7877 可覆盖）
set -u
GO=${GO:-go}
NODES=${NODES:-200}
ROUNDS=${ROUNDS:-5}
PORT=${PORT:-7877}
BASE=http://127.0.0.1:$PORT
APP=${APP:-cap_app}
PW=${PW:-cap123}
CONC=${CONC:-1}
WORK_MS=${WORK:-0}   # 每个任务模拟的业务耗时（毫秒），0=只量派发延迟
# 两种压法：EXEC=BROADCAST 量"200 台节点各一份"；EXEC=MAP + SPLIT=N 量"一台节点同时跑 N 个"。
EXEC=${EXEC:-BROADCAST}
SPLIT=${SPLIT:-0}
TASKS=$NODES
[ "$SPLIT" -gt 0 ] && TASKS=$SPLIT
# 首轮触发前的静置窗口：loadgen 在此期间只维持长轮询+心跳，下面的"空闲态开销"才有落点。
LEAD=${LEAD:-25s}

WORK=$(mktemp -d)
DB=$WORK/cap.db
# DSN 可以整体覆盖（试不同 sqlite pragma、或换 mysql/pgsql 复测同一场景）；默认新建临时库。
DSN=${DSN:-$DB}
BIN=$WORK/gojob-server
GEN=$WORK/gojob-loadgen
pass=0 fail=0
ok()  { pass=$((pass+1)); echo "  PASS  $1"; }
bad() { fail=$((fail+1)); echo "  FAIL  $1"; }
PIDS=()
cleanup() {
  for p in ${PIDS[@]+"${PIDS[@]}"}; do kill -9 "$p" 2>/dev/null; done
  wait 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# 200 个节点 = 200 条长轮询 + 200 条心跳的 keep-alive 连接，再加客户端侧一倍。
# macOS 默认 ulimit -n 常是 256，不抬高会先撞 fd 而不是撞代码上限。
ulimit -n 8192 2>/dev/null || true

jget() {
  printf '%s' "$1" | python3 -c "
import sys,json
try: root=json.load(sys.stdin)
except Exception as e: print('parse-error',e); sys.exit()
try: print($2)
except Exception as e: print('missing',e)
"
}
say() { echo "  ----  $1"; }

echo "== 0. 构建（NODES=$NODES ROUNDS=$ROUNDS CONC=$CONC，fd=$(ulimit -n)）"
cd "$(dirname "$0")/.." || exit 1
$GO build -o "$BIN" ./cmd/gojob-server || { bad "编译 server 失败"; exit 1; }
$GO build -o "$GEN" ./cmd/gojob-loadgen || { bad "编译 loadgen 失败"; exit 1; }
env GOJOB_ADDR=:"$PORT" GOJOB_DB_DRIVER=sqlite GOJOB_DB_DSN=$DSN \
  GOJOB_CLUSTER_SECRET=cap-verify-secret GOJOB_RETENTION_DAYS=1 \
  "$BIN" >"$WORK/server.log" 2>&1 &
PIDS+=($!)
i=0
until curl -sf -m2 "$BASE/healthz" -o /dev/null; do
  i=$((i+1)); [ $i -gt 60 ] && { bad "server 没起来"; tail -20 "$WORK/server.log"; exit 1; }
  sleep 0.25
done
ok "server 就绪"

TK=$(curl -s -m8 -X POST "$BASE/openapi/user/login" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$TK" ] || { bad "登录失败"; exit 1; }
api() { curl -s -m 20 -X POST "$BASE$1" -H 'Content-Type: application/json' -H "X-Gojob-Token: $TK" -d "$2"; }
apiget() { curl -s -m 20 "$BASE$1" -H "X-Gojob-Token: $TK"; }

BODY=$(printf '{"app_id":"%s","app_name":"容量验收","password":"%s"}' "$APP" "$PW")
api /openapi/app/create "$BODY" >/dev/null
# BROADCAST + API 型：一次触发 = 每个在线节点一份；MAP 型：一次触发 = 根派生 SPLIT 个子任务，
# 全部落在同一台节点的空闲额度上（测"一个 worker 同时跑 N 个"）。limit 给足，否则 orDefault 之后
# 并发实例一满就静默 skip（历史上就这么误判过一次"驱动问题"）。
BODY=$(printf '{"app_id":"%s","job_name":"cap-%s","processor":"loadgen","execute_type":"%s","time_expression_type":"API","instance_num_limit":10000,"max_retry_times":0,"instance_retry_num":0,"log_level":"DEBUG"}' "$APP" "$(echo "$EXEC" | tr 'A-Z' 'a-z')" "$EXEC")
R=$(api /openapi/job/create "$BODY")
JOB=$(jget "$R" "root.get('data')")
[ "${JOB:-None}" != "None" ] && ok "创建 $EXEC 任务 id=$JOB" || { bad "建任务失败：$R"; exit 1; }

echo "== 1. 起 $NODES 个真 worker 节点（每台并发额度 $CONC）并等在线数到位"
GOJOB_SESSION_TOKEN=$TK "$GEN" -server "$BASE" -app "$APP" -pw "$PW" -job "$JOB" \
  -nodes "$NODES" -concurrency "$CONC" -split "$SPLIT" -work "${WORK_MS}ms" -rounds "$ROUNDS" -hold 60s -lead "$LEAD" \
  -sdklog "$WORK/sdk.log" >"$WORK/gen.log" 2>&1 &
GENPID=$!
PIDS+=($GENPID)

online=0
i=0
while [ $i -lt 300 ]; do
  online=$(apiget "/openapi/app/workers?app_id=$APP" | python3 -c "import sys,json;print(len(json.load(sys.stdin)['data'] or []))")
  [ "$online" -ge "$NODES" ] && break
  sleep 0.4
  i=$((i+1))
done
[ "$online" -ge "$NODES" ] && ok "在线节点 $online/$NODES" || bad "在线节点只有 $online/$NODES（看 $WORK/gen.log）"

# 冷启动注册风暴的现场证据：整池节点同时打 /worker/register，服务端 accept 队列
# （macOS kern.ipc.somaxconn 默认 128）会溢出，被拒的连接拿到 reset。SDK 现在退避重试，
# 所以这里的数字是"重试了几次"而不是"少了几个节点"。
RET=$(grep -c '注册连不上' "$WORK/sdk.log" 2>/dev/null || true)
 CONV=$(grep -c '注册在第' "$WORK/sdk.log" 2>/dev/null || true)
say "注册退避重试 $RET 次（其中 $CONV 个节点靠重试补上线）；本机 somaxconn=$(sysctl -n kern.ipc.somaxconn 2>/dev/null || echo 未取到)"

say "空闲态（$NODES 节点只挂机、一次未触发）的 Server 开销"
# 副本自心跳是启动后才落库的，早采一行都没有（空到看起来像读取失败），所以先等它上线。
idle_sample() { apiget /openapi/system/nodes | python3 -c "
import sys,json
d=json.load(sys.stdin)['data']
rows=[s for s in d['servers'] if s.get('online')]
if not rows: print('   （副本自心跳还没落库）'); raise SystemExit
for s in rows:
    print('   server: cpu=%s%% heap=%sMB goroutines=%s gc=%s conns(active/idle)=%s/%s 长轮询在途=%s workers_seen=%s' % (
        s.get('cpu_percent'), s.get('heap_inuse_mb'), s.get('goroutines'), s.get('gc_count'),
        s.get('conns_active'), s.get('conns_idle'), s.get('poll_inflight'), len(d.get('workers') or [])))
"; }
i=0
IDLE_OUT=""
while :; do
  IDLE_OUT=$(idle_sample)
  printf '%s' "$IDLE_OUT" | grep -q 'server:' && break
  i=$((i + 1))
  [ $i -ge 25 ] && break
  sleep 0.4
done
# 空闲开销要连采 4 次、只留最后一次：cpu_percent 是"上一个 5s 采样窗口"的均值，
# 刚连满时那个窗口里还压着注册风暴（200 次 bcrypt），第一次读到的 248% 其实是启动开销。
# 早先写成 `idle_sample | grep -q` 的循环把输出喂给了 grep，页面上一行都不剩。
for k in 1 2 3 4; do IDLE_OUT=$(idle_sample); sleep 5; done
printf '%s\n' "$IDLE_OUT"

echo "== 2. 连打 $ROUNDS 轮 $EXEC，每轮 $TASKS 份任务"
# loadgen 会打 round N tasks=... finished=... | 派发延迟 p50=.. p90=.. p99=.. max=.. | 全部跑完=..
while kill -0 $GENPID 2>/dev/null; do sleep 0.5; done
grep -E '^round ' "$WORK/gen.log" || bad "loadgen 没产出轮次结果"

BAD_ROUNDS=$(grep -c 'TIMEOUT' "$WORK/gen.log" || true)
[ "$BAD_ROUNDS" = "0" ] && ok "$ROUNDS 轮全部收齐 $TASKS/$TASKS 份" || bad "有 $BAD_ROUNDS 轮超时"

P50S=$(grep -oE 'p50=[0-9.]+m?s' "$WORK/gen.log" | sed 's/p50=//' | sort -V | tail -1)
MAXS=$(grep -oE 'max=[0-9.]+m?s' "$WORK/gen.log" | sed 's/max=//' | sort -V | tail -1)
say "逐轮分位数（口径=从**发出**触发请求起算，含管理面往返 trigger_http；要净派发延迟就减去它）"
grep '^round ' "$WORK/gen.log" | python3 -c "
import sys,re
def ms(x):
    if x.endswith('ms'): return float(x[:-2])
    if x.endswith('s'):  return float(x[:-1]) * 1000
    return float(x) * 1e6  # Go 的 Duration 原样打印（ns 级）
rows=[]
for line in sys.stdin:
    m=re.match(r'round (\d+).*trigger_http=(\S+) \| 派发延迟 p50=(\S+) p90=(\S+) p99=(\S+) max=(\S+) \| 全部跑完=(\S+)', line)
    if m: rows.append((int(m.group(1)), [ms(m.group(i)) for i in (3,4,5,6)], ms(m.group(2)), ms(m.group(7))))
for r,p,http,span in rows:
    print('   round %-3d p50=%7.1fms p90=%7.1fms p99=%7.1fms max=%7.1fms 全轮跨度=%7.1fms (触发往返 %.1fms)' % (r,p[0],p[1],p[2],p[3],span,http))
if rows:
    w=max(rows, key=lambda x:x[1][3])
    print('   最差一轮 = round %d：p50=%.1fms p99=%.1fms max=%.1fms' % (w[0],w[1][0],w[1][2],w[1][3]))
    print('   中位轮的 max = %.1fms' % sorted(r[1][3] for r in rows)[len(rows)//2])
"

echo "== 3. 压完之后的 Server 开销与实例落库数"
apiget /openapi/system/nodes | python3 -c "
import sys,json
d=json.load(sys.stdin)['data']
for s in d['servers']:
    if not s.get('online'): continue
    print('   server: cpu=%s%% heap=%sMB goroutines=%s gc=%s(%sms) conns=%s 长轮询在途=%s' % (
        s.get('cpu_percent'), s.get('heap_inuse_mb'), s.get('goroutines'), s.get('gc_count'),
        s.get('gc_pause_ms'), s.get('conns_active'), s.get('poll_inflight')))
print('   worker 行数（在线表）=', len(d.get('workers') or []))
" 2>/dev/null
INST=$(apiget "/openapi/instance/list?job_id=$JOB" | python3 -c "import sys,json;print(len(json.load(sys.stdin)['data'] or []))")
say "实例列表返回 $INST 行（接口有 200 行上限，这里只作对照）"

echo
echo "通过 $pass 项，失败 $fail 项 —— 具体延迟数字看上面的 round 行，脚本不对数字做门限判断（阈值该由实测反过来定，不是先定再调）"
[ "$fail" = 0 ] || exit 1
