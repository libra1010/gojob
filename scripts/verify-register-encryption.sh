#!/usr/bin/env bash
# 注册口令加密（一次性 challenge + AES-256-GCM）的实机验收。
#
# 覆盖"配置矩阵 + 攻击面"，而不是把正常路径再跑一遍：
#   未启用 / 已启用可选 / 已启用强制 三档下，明文与加密各自的下场；
#   注册包被重放、换身份冒用、两侧 key 不一致、未知 scheme；
#   同一口令两次注册的密文必须不同；
#   Go SDK 与 Java SDK 各自真跑一次加密注册（真客户端，不是手工拼包）；
#   authApp 同样三档；
#   最后一条硬检查：整轮服务端日志里搜不到项目口令明文。
#
# 手工构造请求体用的是 scripts/register-seal（与服务端同一份线格式实现）。
# 不用 SDK 拼包：用被测方自己证明自己没有意义。
#
# JSON 一律先 printf 拼进变量再 -d "$VAR"：macOS 自带 bash 3.2 在"双引号里嵌 $( ) 再套
# 转义引号"时解析会错乱，实测会把一条 curl 拆成三条、把 JSON 咬掉引号。
#
# 用法: scripts/verify-register-encryption.sh
#   可用 GO/PORT/APP/PW/KEY/OTHER_KEY/JAVA 覆盖；JAVA=0 跳过 Java 那一段。
set -u
GO=${GO:-go}
PORT=${PORT:-7795}
BASE=http://127.0.0.1:$PORT
APP=${APP:-enc_app}
PW=${PW:-enc123}
# 两把 key：一把双方共用，一把只用来造"两侧配的不是同一把"
KEY=${KEY:-000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f}
OTHER_KEY=${OTHER_KEY:-1111111111111111111111111111111111111111111111111111111111111111}
WITH_JAVA=${JAVA:-1}
MVN=${MVN:-$HOME/.m2/wrapper/dists/apache-maven-3.9.10-bin/53h08a94dg6djh6umvruv7q564/apache-maven-3.9.10/bin/mvn}
JAVA_HOME_DIR=${JAVA_HOME:-$HOME/.sdkman/candidates/java/current}
SDK_M2=-Daether.enhancedLocalRepository.trackingFilename=_gojob.repositories

WORK=$(mktemp -d)
DB=$WORK/enc.db
BIN=$WORK/gojob-server
SEAL=$WORK/register-seal
DEMOBIN=$WORK/gojob-demo
pass=0 fail=0
ok()  { pass=$((pass+1)); echo "  PASS  $1"; }
bad() { fail=$((fail+1)); echo "  FAIL  $1"; }
PIDS=()
LOGN=0
LOGIN_TK=
SERVER_LOG=

cleanup() {
  for p in ${PIDS[@]+"${PIDS[@]}"}; do kill -9 "$p" 2>/dev/null; done
  wait 2>/dev/null   # 回收，否则脚本退出时 bash 会把 "Killed: 9" 打到用户脸上
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
want_code() { # want_code <说明> <期望 code> <json>
  local got; got=$(jget "$3" "root['code']")
  if [ "$got" = "$2" ]; then ok "$1 (code=$got)"; else bad "$1 期望 code=$2 实得 $got -> $3"; fi
}
want_txt() { # want_txt <说明> <子串> <文本>
  if printf '%s' "$3" | grep -qi -- "$2"; then ok "$1"; else
    bad "$1：找不到「$2」-> $(printf '%s' "$3" | head -c 400)"
  fi
}
not_txt() { # not_txt <说明> <不该出现的子串> <文本>
  if printf '%s' "$3" | grep -qi -- "$2"; then bad "$1：出现了「$2」"; else ok "$1"; fi
}
want_log() { want_txt "$1" "$2" "$(cat "$3" 2>/dev/null)"; }
not_log()  { not_txt  "$1" "$2" "$(cat "$3" 2>/dev/null)"; }

wreg()   { curl -s -m 8 -X POST "$BASE/worker/register" -H 'Content-Type: application/json' -d "$1"; }
wcall()  { curl -s -m 8 -X POST "$BASE$1" -H 'Content-Type: application/json' -d "$2"; }
pjcall() { curl -s -m 8 -X POST "$BASE$1" -H 'Content-Type: application/json' -d "$2"; }
api()    { curl -s -m 8 -X POST "$BASE$1" -H 'Content-Type: application/json' -H "X-Gojob-Token: $LOGIN_TK" -d "$2"; }
apiget() { curl -s -m 8 "$BASE$1" -H "X-Gojob-Token: $LOGIN_TK"; }

start_server() { # start_server <额外 env（形如 "A=1 B=2"，可为空）>
  LOGN=$((LOGN+1))
  SERVER_LOG=$WORK/server-$LOGN.log
  # shellcheck disable=SC2086
  env GOJOB_ADDR=:"$PORT" GOJOB_DB_DRIVER=sqlite GOJOB_DB_DSN=$DB \
      GOJOB_CLUSTER_SECRET=enc-verify-secret $1 "$BIN" >"$SERVER_LOG" 2>&1 &
  PIDS+=($!)
  local i=0
  until curl -sf -m 2 "$BASE/healthz" -o /dev/null; do
    i=$((i+1))
    if [ $i -gt 80 ]; then bad "服务端没起来"; tail -20 "$SERVER_LOG"; exit 1; fi
    sleep 0.25
  done
}
stop_all() {
  for p in ${PIDS[@]+"${PIDS[@]}"}; do kill -9 "$p" 2>/dev/null; done
  wait 2>/dev/null
  PIDS=()
}
login() {
  LOGIN_TK=$(curl -s -m 8 -X POST "$BASE/openapi/user/login" -H 'Content-Type: application/json' \
    -d '{"username":"admin","password":"admin123"}' | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  [ -n "$LOGIN_TK" ] || { bad "登录失败"; exit 1; }
}
# sealed <instance_id> <key> [challenge] → 一个加密注册体（不给 challenge 时自己去领一张）
sealed() {
  if [ $# -ge 3 ]; then
    "$SEAL" -base "$BASE" -app "$APP" -inst "$1" -pw "$PW" -key "$2" -challenge "$3"
  else
    "$SEAL" -base "$BASE" -app "$APP" -inst "$1" -pw "$PW" -key "$2"
  fi
}
plain_body() { # plain_body <instance_id> → 明文口令形态的注册体
  "$SEAL" -base "$BASE" -plain -app "$APP" -inst "$1" -pw "$PW" -key "$KEY"
}
worker_heartbeat() { # worker_heartbeat <instance_id> <token>
  local body
  body=$(printf '{"instance_id":"%s","app_id":"%s","address":"probe:1","cpu_load":0,"cpu_processors":8,"memory_used":10,"memory_max":0,"tag":"probe","version":"e2e","sdk_type":"GO","token":"%s"}' "$1" "$APP" "$2")
  wcall /worker/heartbeat "$body"
}
wait_online() { # wait_online <匹配串> —— 等在线表里出现这个特征的节点
  # 特征必须只属于被测进程本身：前几段手工构造的注册体与探针心跳都是 sdk_type=GO，
  # 拿它当特征会"立刻通过"，而真 worker 还没起来（实例就一直 WAITING，日志也还是空的）。
  # demo worker 带 tag=enc（探针心跳用 tag=probe），Java worker 是唯一会报 JAVA 的进程。
  local i=0
  until apiget "/openapi/app/workers?app_id=$APP" | grep -q "$1"; do
    i=$((i+1)); [ $i -gt 60 ] && return 1; sleep 0.5
  done
  return 0
}

echo "== 0. 构建"
cd "$(dirname "$0")/.." || exit 1
$GO build -o "$BIN" ./cmd/gojob-server || { bad "编译 gojob-server 失败"; exit 1; }
$GO build -o "$SEAL" ./scripts/register-seal || { bad "编译 register-seal 失败"; exit 1; }
$GO build -o "$DEMOBIN" ./cmd/gojob-demo || { bad "编译 gojob-demo 失败"; exit 1; }
ok "编译 gojob-server / register-seal / gojob-demo"
KID=$("$SEAL" -key "$KEY" -kid-only)
if [ "$KID" = "630dcd29" ]; then ok "线格式与黄金向量一致（kid=$KID）"; else bad "kid=$KID，期望 630dcd29"; fi

echo "== 1. 未启用加密：行为与引入本功能前一致"
start_server ""
login
BODY=$(printf '{"app_id":"%s","app_name":"加密验收","password":"%s"}' "$APP" "$PW")
want_code "创建项目" 0 "$(api /openapi/app/create "$BODY")"
BODY=$(printf '{"app_id":"%s","job_name":"enc-manual","processor":"hello","time_expression_type":"API","instance_num_limit":100}' "$APP")
want_code "创建 API 型任务" 0 "$(api /openapi/job/create "$BODY")"
JOB_ID=$(apiget "/openapi/job/list?app_id=$APP&page_size=0" | python3 -c "
import sys,json
d={j['job_name']:j['id'] for j in json.load(sys.stdin)['data']['data']}
print(d.get('enc-manual'))")

R=$(wcall /worker/challenge "{\"app_id\":\"$APP\",\"instance_id\":\"i0\"}")
want_txt "未启用时 /worker/challenge 明确拒绝并点名配置项" "GOJOB_WORKER_AES_KEYS" "$R"
want_code "未启用时明文注册照常成功" 0 "$(wreg "$(plain_body i0)")"
MD5=$(printf '0%.0s' $(seq 32))
BODY=$(printf '{"appName":"%s","encryptedPassword":"%s","encryptType":1}' "$APP" "$MD5")
want_txt "authApp 的 md5 形态仍被明确拒绝" "md5" "$(pjcall /openApi/authApp "$BODY")"
BODY=$(printf '{"appName":"%s","password":"%s"}' "$APP" "$PW")
want_txt "未启用时 authApp 明文照常可用" '"token"' "$(pjcall /openApi/authApp "$BODY")"

echo "== 2. 滚到「已启用、可明文」这一档（分批升级期）"
stop_all
start_server "GOJOB_WORKER_AES_KEYS=$KEY"
want_log "启动日志报告加密已开" "worker register encryption" "$SERVER_LOG"
want_log "启动日志列出接受的 kid" "$KID" "$SERVER_LOG"
not_log "启动日志不打印 key 本身" "$KEY" "$SERVER_LOG"
want_code "明文仍可注册" 0 "$(wreg "$(plain_body p1)")"
B1=$(sealed e1 "$KEY")
R=$(wreg "$B1")
TK_E=$(jget "$R" "root['data']['token']")
[ ${#TK_E} -ge 16 ] && ok "加密注册成功并换到 token" || bad "加密注册失败：$R"
not_txt "加密注册体里没有明文口令" "$PW" "$B1"
want_txt "注册体带的是 aes-gcm 方案" "aes-gcm" "$B1"
want_code "加密换来的 token 能心跳" 0 "$(worker_heartbeat e1 "$TK_E")"

echo "== 3. 抓包重放与换身份冒用"
R=$(wreg "$B1")
want_code "同一个注册包原样重放被拒" 401 "$R"
want_txt "拒绝理由指向 challenge 一次性" "challenge" "$R"
B3=$(sealed e3 "$KEY")
# 密文里绑死了 instance_id，但外层表头是请求者可控的——挪到别人名下必须被拒
BAD=$(printf '%s' "$B3" | python3 -c "import sys,json;d=json.load(sys.stdin);d['instance_id']='victim';print(json.dumps(d))")
want_code "把密文配到别人的 instance_id 上冒用被拒" 401 "$(wreg "$BAD")"

echo "== 4. 同一口令两次注册：密文必然不同"
B2=$(sealed e1 "$KEY")
C1=$(printf '%s' "$B1" | python3 -c "import sys,json;print(json.load(sys.stdin)['ciphertext'])")
C2=$(printf '%s' "$B2" | python3 -c "import sys,json;print(json.load(sys.stdin)['ciphertext'])")
if [ "$C1" != "$C2" ]; then ok "两次密文不同（抓包比不出「同一节点/同一口令」）"; else bad "两次密文相同：iv 没随机"; fi
want_code "第二包照常注册成功" 0 "$(wreg "$B2")"

echo "== 5. 两侧 key 不一致 / 未知 scheme：文案指向配置而不是口令"
R=$(wreg "$(sealed e4 "$OTHER_KEY")")
want_code "用另一把 key 加密被拒" 401 "$R"
want_txt "理由指向 unknown key id" "unknown key id" "$R"
not_txt "理由里不含 password（别把运维引去核对口令）" "password" "$R"
BODY=$(printf '{"instance_id":"e5","app_id":"%s","auth_scheme":"aes-128-cbc"}' "$APP")
want_txt "未知 scheme 被明确拒绝" "unknown auth_scheme" "$(wreg "$BODY")"

echo "== 6. Go SDK 真客户端：配了 key 就不再上链路传口令"
DLOG=$WORK/go-worker.log
GOJOB_WORKER_SERVER=$BASE GOJOB_WORKER_APP=$APP GOJOB_WORKER_PASSWORD=$PW \
GOJOB_WORKER_AES_KEY=$KEY GOJOB_WORKER_DRAIN_ADDR=127.0.0.1:7895 \
  "$DEMOBIN" -tag enc >"$DLOG" 2>&1 &
PIDS+=($!)
if wait_online '"tag":"enc"'; then ok "Go worker 以加密注册上线"; else bad "Go worker 没上线：$(tail -5 "$DLOG")"; fi
want_log "Go worker 日志确认走的是加密握手" "encrypted=true" "$DLOG"
not_log "Go worker 日志里没有明文口令" "$PW" "$DLOG"
BODY=$(printf '{"job_id":%s,"action":"trigger"}' "$JOB_ID")
api /openapi/job/operate "$BODY" >/dev/null
ST=
i=0
while [ $i -lt 40 ]; do
  ST=$(apiget "/openapi/instance/list?job_id=$JOB_ID" | python3 -c "import sys,json;d=json.load(sys.stdin)['data'] or [];print(d[0]['status'] if d else '')")
  case "$ST" in SUCCEED|FAILED|STOPPED) break ;; esac
  i=$((i+1)); sleep 0.5
done
[ "$ST" = "SUCCEED" ] && ok "加密注册换来的票能领任务并跑成功" || bad "实例终态 = $ST"

echo "== 7. 强制档：明文 428（不是 401）并附可用 challenge"
stop_all
start_server "GOJOB_WORKER_AES_KEYS=$KEY GOJOB_WORKER_AES_REQUIRED=true"
login
R=$(wreg "$(plain_body f1)")
want_code "强制下明文注册被拒" 428 "$R"
want_txt "理由是「本部署要求加密注册」" "requires encrypted" "$R"
CH=$(jget "$R" "root['data']['challenge']")
[ ${#CH} -gt 16 ] && ok "428 顺手给了一张 challenge（客户端省一次往返）" || bad "428 没带 challenge：$R"
want_txt "schemes 只列 aes-gcm" "aes-gcm" "$R"
want_code "用 428 给的 challenge 改加密重来即成功" 0 "$(wreg "$(sealed f1 "$KEY" "$CH")")"
want_code "这张 challenge 同样只用一次" 401 "$(wreg "$(sealed f1 "$KEY" "$CH")")"
# 没配 key 的 worker 在强制档下应当启动即失败，并把原因说清楚
NWLOG=$WORK/go-worker-nokey.log
GOJOB_WORKER_SERVER=$BASE GOJOB_WORKER_APP=$APP GOJOB_WORKER_PASSWORD=$PW \
GOJOB_WORKER_DRAIN_ADDR=127.0.0.1:7896 "$DEMOBIN" -tag nokey >"$NWLOG" 2>&1 &
wait $! 2>/dev/null
want_log "没配 key 的 worker 注册失败并指向 worker.Config.AesKey" "AesKey" "$NWLOG"

echo "== 8. authApp 同样三档"
BODY=$(printf '{"appName":"%s","password":"%s"}' "$APP" "$PW")
R=$(pjcall /openApi/authApp "$BODY")
want_txt "强制下 authApp 明文被拒" "requires encrypted" "$R"
CH=$(printf '%s' "$R" | python3 -c "import sys,json;print((json.load(sys.stdin).get('data') or {}).get('challenge',''))")
[ -n "$CH" ] && ok "authApp 的拒绝里带了下一步要用的 challenge" || bad "authApp 拒绝没带 challenge：$R"
BODY=$("$SEAL" -base "$BASE" -openapi -app "$APP" -pw "$PW" -key "$KEY" -challenge "$CH")
not_txt "authApp 的加密请求体里没有明文口令" "$PW" "$BODY"
R=$(pjcall /openApi/authApp "$BODY")
TOK_PJ=$(printf '%s' "$R" | python3 -c "import sys,json;print((json.load(sys.stdin).get('data') or {}).get('token',''))")
[ ${#TOK_PJ} -ge 16 ] && ok "authApp 走加密握手换到 OpenAPI 票" || bad "authApp 加密失败：$R"
R=$(curl -s -m 8 "$BASE/openApi/fetchAllJob" -H "X-POWERJOB-ACCESS-TOKEN: $TOK_PJ" -d '{}')
want_txt "这张票真能用（兼容层读得到任务）" "enc-manual" "$R"

echo "== 9. Java SDK 真客户端（另一门语言走同一条握手）"
if [ "$WITH_JAVA" != "1" ]; then
  echo "  SKIP  Java 段（JAVA=0）"
elif [ ! -x "$MVN" ]; then
  bad "找不到 Maven（$MVN），Java 段没验成"
else
  export JAVA_HOME=$JAVA_HOME_DIR
  ( cd sdk-java && "$MVN" -o -q $SDK_M2 -pl gojob-worker-core test ) >"$WORK/java-test.log" 2>&1 \
    && ok "Java 单测通过（含用 Java 解 Go 的黄金密文）" || bad "Java 单测失败：$(tail -20 "$WORK/java-test.log")"
  ( cd sdk-java && "$MVN" -o -q $SDK_M2 -pl gojob-worker-core dependency:build-classpath \
      -Dmdep.outputFile="$WORK/cp.txt" ) >/dev/null 2>&1
  if [ -s "$WORK/cp.txt" ]; then
    JLOG=$WORK/java-worker.log
    GOJOB_WORKER_SERVER=$BASE GOJOB_WORKER_APP=$APP GOJOB_WORKER_PASSWORD=$PW \
    GOJOB_WORKER_AES_KEY=$KEY \
      "$JAVA_HOME_DIR/bin/java" \
        -cp "sdk-java/gojob-worker-core/target/classes:sdk-java/gojob-worker-core/target/test-classes:$(cat "$WORK/cp.txt")" \
        com.gojob.worker.examples.QuickStart >"$JLOG" 2>&1 &
    PIDS+=($!)
    if wait_online '"sdk_type":"JAVA"'; then ok "Java worker 以加密注册上线（跨语言口径一致）"; else
      bad "Java worker 没上线：$(tail -8 "$JLOG")"
    fi
    want_log "Java worker 日志确认走的是加密握手" "encrypted=true" "$JLOG"
    not_log "Java worker 日志里没有明文口令" "$PW" "$JLOG"
  else
    bad "拿不到 Java classpath，Java 段没验成"
  fi
fi

echo "== 10. 硬检查：整轮服务端日志里搜不到项目口令明文"
cat $WORK/server-*.log >"$WORK/all-server.log" 2>/dev/null
not_txt "服务端日志全程不含项目口令（明文档 + 强制档整轮）" "$PW" "$(cat "$WORK/all-server.log")"
n=$(grep -c "worker register encryption" "$WORK/all-server.log" 2>/dev/null || echo 0)
[ "$n" -ge 2 ] && ok "服务端两次以加密档启动过（可选 + 强制），共 $n 行" || bad "加密启动日志行数 = $n"

echo
echo "通过 $pass 项，失败 $fail 项"
[ "$fail" = 0 ] || exit 1
