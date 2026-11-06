// 实机验收用的手工注册载荷生成器（不是发布物，只服务 scripts/verify-register-encryption.sh）。
//
// 为什么需要它：加密注册的两步握手要在 bash 里算 AES-GCM/HKDF 不现实（macOS 自带没有
// 可用的加密 CLI），而"抓包重放""两侧 key 不一致"这些断言又必须手工构造请求体，
// 不能整包交给 SDK —— 交给 SDK 就等于用被测方自己证明自己。
//
// 它只做客户端该做的那半步（领 challenge + 按线格式加密），把拼好的 JSON 打到 stdout：
//
//	go run ./scripts/register-seal -base http://127.0.0.1:7795 -app enc_app -inst i1 \
//	    -pw enc123 -key <64 位 hex>            # → {"instance_id":...,"auth_scheme":"aes-gcm",...}
//	... -challenge <服务端刚给的 challenge>    # 跳过领取，直接用这张（验 428 提示里附的票）
//	... -plain                                # → 明文口令形态
//	... -openapi                              # → authApp 形态（camelCase，无 instance_id）
//	... -kid-only                             # 只打印这把 key 的 kid，不联网
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/internal/registercrypto"
)

var (
	base      = flag.String("base", "http://127.0.0.1:7800", "server 地址")
	app       = flag.String("app", "", "app_id / appName")
	inst      = flag.String("inst", "", "worker instance_id")
	pw        = flag.String("pw", "", "项目口令明文")
	key       = flag.String("key", "", "AES-256 key（64 位 hex）")
	ch        = flag.String("challenge", "", "已有 challenge，留空则现领一张")
	isOpenAPI = flag.Bool("openapi", false, "生成 authApp 形态的请求体")
	isPlain   = flag.Bool("plain", false, "生成明文口令形态（不加密）")
	isKidOnly = flag.Bool("kid-only", false, "只打印这把 key 的 kid 并退出")
)

func main() {
	flag.Parse()
	raw, err := registercrypto.ParseKey(*key)
	if err != nil && !*isPlain {
		die("%v", err)
	}
	if *isKidOnly {
		fmt.Println(registercrypto.KeyId(raw))
		return
	}
	if *app == "" || *pw == "" {
		die("-app 与 -pw 必填")
	}
	if *isPlain {
		emit(workerBody())
		return
	}
	// OpenAPI 侧没有实例身份（principal 为空），所以载荷里的 instance_id 也留空
	principal := *inst
	if *isOpenAPI {
		principal = ""
	}
	challenge := *ch
	if challenge == "" {
		challenge = fetchChallenge()
	}
	kid, iv, ct, err := registercrypto.Seal(raw, registercrypto.Payload{
		Version: registercrypto.PayloadVersion, AppId: *app, InstanceId: principal,
		Challenge: challenge, Password: *pw,
	})
	if err != nil {
		die("seal: %v", err)
	}
	if *isOpenAPI {
		emit(map[string]any{"appName": *app, "authScheme": registercrypto.SchemeAesGcm,
			"kid": kid, "iv": iv, "ciphertext": ct, "challenge": challenge})
		return
	}
	body := workerBody()
	body["auth_scheme"], body["kid"], body["iv"], body["ciphertext"], body["challenge"] =
		registercrypto.SchemeAesGcm, kid, iv, ct, challenge
	delete(body, "password") // 口令只存在于密文里
	emit(body)
}

func workerBody() map[string]any {
	if *isOpenAPI {
		return map[string]any{"appName": *app, "password": *pw}
	}
	if *inst == "" {
		die("-inst 必填（或用 -openapi）")
	}
	return map[string]any{"instance_id": *inst, "app_id": *app,
		"address": "seal-tool:1", "version": "seal-tool", "sdk_type": "GO",
		"auth_scheme": registercrypto.SchemePlain, "password": *pw}
}

func fetchChallenge() string {
	path, body := "/worker/challenge", map[string]any{"app_id": *app, "instance_id": *inst}
	if *isOpenAPI {
		path, body = "/openApi/challenge", map[string]any{"appName": *app}
	}
	bs, err := v2.Marshal(body)
	if err != nil {
		die("marshal: %v", err)
	}
	resp, err := http.Post(*base+path, "application/json", bytes.NewReader(bs))
	if err != nil {
		die("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		die("read: %v", err)
	}
	if *isOpenAPI { // 兼容信封是 {success,data{challenge},message}，没有 code
		var r struct {
			Success bool `json:"success"`
			Data    struct {
				Challenge string `json:"challenge"`
			} `json:"data"`
			Message string `json:"message"`
		}
		if err := v2.Unmarshal(rawBody, &r); err != nil {
			die("decode %s: %v (%s)", path, err, rawBody)
		}
		if r.Data.Challenge == "" {
			die("%s 没拿到 challenge: %s", path, rawBody)
		}
		return r.Data.Challenge
	}
	var r protocol.Resp[protocol.ChallengeResp]
	if err := v2.Unmarshal(rawBody, &r); err != nil {
		die("decode %s: %v (%s)", path, err, rawBody)
	}
	if r.Code != protocol.CodeOK || r.Data.Challenge == "" {
		die("%s 没拿到 challenge: code=%d msg=%q", path, r.Code, r.Message)
	}
	return r.Data.Challenge
}

func emit(m map[string]any) {
	bs, err := v2.Marshal(m)
	if err != nil {
		die("marshal: %v", err)
	}
	fmt.Print(string(bs))
}

func die(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(1)
}
