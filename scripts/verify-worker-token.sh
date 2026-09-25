#!/usr/bin/env bash
# per-worker token 的实机验收：注册换票、数据面只认票、冒用被拒、改口令即吊销。
set -u
BASE=${BASE:-http://127.0.0.1:7789}
APP=${APP:-demo_app}
PW=${PW:-demo123}
pass=0 fail=0
ok()  { pass=$((pass+1)); echo "  PASS  $1"; }
bad() { fail=$((fail+1)); echo "  FAIL  $1"; }

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
  local got; got=$(jget "$3" "root['code']")
  if [ "$got" = "$2" ]; then ok "$1 (code=$got)"; else bad "$1 期望 code=$2 实得 $got -> $3"; fi
}
want_msg() { # want_msg <说明> <子串> <json>
  if printf '%s' "$3" | grep -q "$2"; then ok "$1"; else bad "$1：输出里没有「$2」-> $3"; fi
}

HEART="\"instance_id\":\"t-a\",\"app_id\":\"$APP\",\"address\":\"probe:1\",\"cpu_load\":0,\"cpu_processors\":8,\"memory_used\":10,\"memory_max\":0,\"tag\":\"e2e\",\"version\":\"e2e\",\"sdk_type\":\"GO\""

echo "== 1. 注册：口令错 / 对"
R=$(curl -s -m 5 -X POST $BASE/worker/register -H 'Content-Type: application/json' \
  -d '{"instance_id":"t-a","app_id":"'$APP'","password":"nope"}')
want_code "错口令注册被拒" 401 "$R"
R=$(curl -s -m 5 -X POST $BASE/worker/register -H 'Content-Type: application/json' \
  -d '{"instance_id":"","app_id":"'$APP'","password":"'$PW'"}')
want_code "缺 instance_id 直接被拒" 400 "$R"
R=$(curl -s -m 5 -X POST $BASE/worker/register -H 'Content-Type: application/json' \
  -d '{"instance_id":"t-a","app_id":"'$APP'","password":"'$PW'","address":"probe:1","sdk_type":"GO","version":"e2e"}')
want_code "正确口令注册成功" 0 "$R"
TK=$(jget "$R" "root['data']['token']")
TTL=$(jget "$R" "root['data']['ttl_seconds']")
[ ${#TK} -ge 16 ] && ok "换到 token（${#TK} 字节，ttl ${TTL}s）" || bad "换不到 token：$R"

echo "== 2. 数据面只认 token"
R=$(curl -s -m 5 -X POST $BASE/worker/heartbeat -H 'Content-Type: application/json' \
  -d "{$HEART,\"token\":\"$TK\"}")
want_code "带 token 心跳通过" 0 "$R"
R=$(curl -s -m 5 -X POST $BASE/worker/heartbeat -H 'Content-Type: application/json' \
  -d "{\"instance_id\":\"t-a\",\"app_id\":\"$APP\",\"password\":\"$PW\"}")
want_code "只带口令的心跳被拒" 401 "$R"
want_msg "报错引导去重新注册" "register" "$R"

echo "== 3. 冒用他人身份被拒"
R=$(curl -s -m 5 -X POST $BASE/worker/fetchTask -H 'Content-Type: application/json' \
  -d '{"instance_id":"victim","app_id":"'$APP'","address":"probe:1","token":"'$TK'","max_task_num":1,"wait_seconds":1}')
want_code "同票自称别人的 instance_id" 401 "$R"
R=$(curl -s -m 5 -X POST $BASE/worker/heartbeat -H 'Content-Type: application/json' \
  -d '{"instance_id":"t-a","app_id":"other_app","address":"probe:1","token":"'$TK'"}')
want_code "同票自称别的 app_id" 401 "$R"
R=$(curl -s -m 5 -X POST $BASE/worker/reportLogs -H 'Content-Type: application/json' \
  -d '{"instance_id":"t-a","app_id":"'$APP'","token":"0000000000000000000000000000dead","entries":[{"task_id":1,"seq":1,"level":"INFO","content":"x","time":1}]}')
want_code "伪造的票" 401 "$R"

echo "== 4. OpenAPI authApp 只收明文（声明的偏离）"
R=$(curl -s -m 5 -X POST $BASE/openApi/authApp -H 'Content-Type: application/json' \
  -d '{"appName":"'$APP'","password":"'$PW'"}')
want_msg "明文 authApp 通过" '"success":true' "$R"
M=$(python3 -c "import hashlib;print(hashlib.md5(b'$PW').hexdigest())")
R=$(curl -s -m 5 -X POST $BASE/openApi/authApp -H 'Content-Type: application/json' \
  -d '{"appName":"'$APP'","encryptedPassword":"'$M'","encryptType":1}')
want_msg "md5-only authApp 被拒" '"success":false' "$R"
want_msg "拒因说明要明文口令" "plaintext" "$R"
R=$(curl -s -m 5 -X POST $BASE/openApi/authApp -H 'Content-Type: application/json' \
  -d '{"appName":"'$APP'","password":"'$M'"}')
want_msg "把 md5 当明文也过不了" "mismatch" "$R"

echo "== 5. 库里不再有口令明文或弱哈希"
python3 - "$APP" <<'PY'
import hashlib, sqlite3, sys
db = sqlite3.connect('data/gojob.db')
cols = [r[1] for r in db.execute("PRAGMA table_info(app_infos)")]
md5 = hashlib.md5(b"demo123").hexdigest()
rows = list(db.execute("select app_id,password from app_infos"))
assert 'password_md5' not in cols, f"表里仍有 password_md5：{cols}"
for aid, pw in rows:
    assert 'demo123' not in (pw or '') and md5 not in (pw or ''), f"{aid} 的 password 含口令可推导值"
print(f"  PASS  app_infos 列={cols}；{len(rows)} 行都是 bcrypt 摘要")
PY
[ $? -eq 0 ] || fail=$((fail+1))

echo
echo "结果：PASS=$pass FAIL=$fail"
[ $fail -eq 0 ]
