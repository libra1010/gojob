// 注册口令的加解密线格式（wire format）：唯一的一份 Go 实现，服务端与 Go SDK 都从这里取，
// 所以两侧不可能各自演进成两套字节口径；Java SDK 由 golden 向量测试钉住同一口径
// （见 vector_test.go 里那组固定 key/iv/明文的期望密文）。
//
// 口径（三处必须逐字一致：Go server、Go SDK、Java SDK）：
//   - key 派生：HKDF-SHA256(ikm=配置的 AES key, salt="gojob-register:v1|"+app_id,
//     info="gojob-worker-register", 32 字节)
//   - 明文：RegisterPayload 的 JSON（UTF-8）
//   - 密文：AES-256-GCM，12 字节随机 iv **单独传**、认证标签附在密文尾部（JCE 的默认约定）
//   - AAD："gojob-register:v1\n" + kid + "\n" + app_id
//   - kid：sha256(key) 的前 4 字节 hex
//   - iv / 密文上线时各自 base64（标准表、带填充）
//
// salt 带 app_id：同一把全局 key 在不同项目下派生出不同的 AES key，所以"拿到一个项目的
// 注册密文"连跨项目冒用的可能性都没有。
package registercrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"

	v2 "encoding/json/v2"
)

// PayloadVersion 写进 AAD、HKDF salt 与明文载荷，三处同源。
// 将来换算法或换 salt 口径就开一个新值，让服务端能按版本分流，而不是让旧客户端静默失败。
const PayloadVersion = "gojob-register:v1"

const (
	SchemePlain  = "plain"
	SchemeAesGcm = "aes-gcm"
)

// ErrDecryptFailed 唯一的一条解密失败文案：key 不对、内容被改过、版本不符都走这里。
// 故意不区分——区分就是把 oracle 递出去（CBC + 分别报错 = padding oracle 的教科书成因）。
var ErrDecryptFailed = errors.New("decrypt failed: wrong key, tampered payload, or version mismatch")

// Payload 加密段里真正承载的内容。用 JSON 而不是分隔符拼接：
// instance_id 由 worker 自拟，任何转义规则都会成为新的歧义源。
type Payload struct {
	Version    string `json:"v"`
	AppId      string `json:"app_id"`
	InstanceId string `json:"instance_id"`
	Challenge  string `json:"challenge"`
	Password   string `json:"password"`
}

// KeyId 由 key 本身算出的指纹（sha256 前 4 字节 hex）。
// kid 一律派生、不许手写：两侧只配 key 就能互认，写错 kid 只会多一种难查的错配。
// 它会随每次注册请求上线，但它不是秘密——它是单向摘要，只用来"两侧核对配的是不是同一把"。
func KeyId(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:4])
}

// ParseKey 解析配置里的 key：64 位 hex（AES-256）。允许大小写混写，但不允许别的长度——
// 长度错一位就是另一套算法，静默接受只会让两侧对不上。
func ParseKey(spec string) ([]byte, error) {
	raw, err := hex.DecodeString(spec)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("AES key must be 64 hex chars (32 bytes, AES-256)")
	}
	return raw, nil
}

func deriveKey(raw []byte, appId string) ([]byte, error) {
	return hkdf.Key(sha256.New, raw, []byte(PayloadVersion+"|"+appId), "gojob-worker-register", 32)
}

func aad(kid, appId string) []byte {
	return []byte(PayloadVersion + "\n" + kid + "\n" + appId)
}

func newGCM(raw []byte, appId, kid string) (cipher.AEAD, error) {
	aesKey, err := deriveKey(raw, appId)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal 加密一份载荷。kid 由 key 算出，所以调用方不用（也不该）自己填。
// 每次调用都用新的随机 iv ⇒ 同一口令注册两次，密文必然不同。
func Seal(raw []byte, p Payload) (kid, ivB64, ctB64 string, err error) {
	kid = KeyId(raw)
	gcm, err := newGCM(raw, p.AppId, kid)
	if err != nil {
		return "", "", "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", "", "", err
	}
	body, err := v2.Marshal(p)
	if err != nil {
		return "", "", "", err
	}
	ct := gcm.Seal(nil, iv, body, aad(kid, p.AppId))
	return kid, base64.StdEncoding.EncodeToString(iv), base64.StdEncoding.EncodeToString(ct), nil
}

// SealWithIv 只给测试用：固定 iv 才能算出可复现的期望密文（golden 向量）。
// 生产路径必须走 Seal——GCM 的 nonce 重用会直接毁掉机密性与完整性。
func SealWithIv(raw, iv []byte, p Payload) (kid, ivB64, ctB64 string, err error) {
	kid = KeyId(raw)
	gcm, err := newGCM(raw, p.AppId, kid)
	if err != nil {
		return "", "", "", err
	}
	body, err := v2.Marshal(p)
	if err != nil {
		return "", "", "", err
	}
	ct := gcm.Seal(nil, iv, body, aad(kid, p.AppId))
	return kid, base64.StdEncoding.EncodeToString(iv), base64.StdEncoding.EncodeToString(ct), nil
}

// Open 解密并校验版本与 app_id。raw 是**服务端已经按 kid 挑好的那把 key**；
// 没有"未知 kid"这类分支，密钥选择是调用方（KeyRing）的职责。
func Open(raw []byte, kid, appId, ivB64, ctB64 string) (Payload, error) {
	iv, err := base64.StdEncoding.DecodeString(ivB64)
	if err != nil {
		return Payload{}, ErrDecryptFailed
	}
	ct, err := base64.StdEncoding.DecodeString(ctB64)
	if err != nil {
		return Payload{}, ErrDecryptFailed
	}
	gcm, err := newGCM(raw, appId, kid)
	if err != nil {
		return Payload{}, err
	}
	if len(iv) != gcm.NonceSize() {
		return Payload{}, ErrDecryptFailed
	}
	body, err := gcm.Open(nil, iv, ct, aad(kid, appId))
	if err != nil {
		return Payload{}, ErrDecryptFailed
	}
	var p Payload
	if err := v2.Unmarshal(body, &p); err != nil {
		return Payload{}, ErrDecryptFailed
	}
	if p.Version != PayloadVersion {
		return Payload{}, ErrDecryptFailed
	}
	if p.AppId != appId {
		return p, errors.New("encrypted payload does not match app_id")
	}
	return p, nil
}
