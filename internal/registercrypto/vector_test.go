// 线格式的黄金向量（golden vector）：口径一旦漂移，这个测试必须红。
//
// 为什么值得单独钉一遍：服务端、Go SDK、Java SDK 三方要对同一串字节达成一致，
// 而"两边各自实现、互相能解通"只覆盖了 Go↔Go。这里那组期望值是用 **Node 的
// crypto（OpenSSL 实现）** 独立算出来的，与 Go 标准库无共享代码；Java SDK 的单元测试
// 钉的是同一组数（JCE 没有 HKDF，得手写，手写最容易错位）。
// 于是"三方一致"变成"三方各自对齐一个外部参照"。
package registercrypto

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	v2 "encoding/json/v2"
)

const (
	vecKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	vecIvHex  = "abababababababababababab"
	// 明文载荷的字节布局也是口径的一部分：字段顺序 = Payload 的声明顺序，紧凑无空白。
	vecPlain = `{"v":"gojob-register:v1","app_id":"app1","instance_id":"i1","challenge":"ch1","password":"pw1"}`
	vecCtB64 = "3oespKc2ZiWBeiCY1B+kJEyY8uEDyJrJCpstWAHbXZCgSWA8gTJeFnY3nF/197Hee7F3" +
		"jDFVfJ7s16BbheF80UgazSreTNscs3AUX2Wpt8unfyOkraO1Oglmt9RBXg0Gzb1IEiyTZFWJKbOqANyo"
)

func vecPayload() Payload {
	return Payload{
		Version: PayloadVersion, AppId: "app1", InstanceId: "i1",
		Challenge: "ch1", Password: "pw1",
	}
}

func vecKey(t *testing.T) []byte {
	t.Helper()
	raw, err := ParseKey(vecKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 明文 JSON 的字节顺序变了（改结构体字段顺序、改 tag），Java 侧的期望密文会集体失效。
func TestPayloadJsonLayout(t *testing.T) {
	bs, err := v2.Marshal(vecPayload())
	if err != nil {
		t.Fatal(err)
	}
	if string(bs) != vecPlain {
		t.Fatalf("明文载荷字节漂移:\n got %s\nwant %s", bs, vecPlain)
	}
}

func TestDerivedKeyVector(t *testing.T) {
	key := vecKey(t)
	if got := KeyId(key); got != "630dcd29" {
		t.Fatalf("kid = %s, want 630dcd29", got)
	}
	dk, err := deriveKey(key, "app1")
	if err != nil {
		t.Fatal(err)
	}
	// Node: crypto.hkdfSync("sha256", key, "gojob-register:v1|app1", "gojob-worker-register", 32)
	const want = "a5c986d6b858a46df5db12820b41522e8c8c12039ec51b9c96762398ae4c8a81"
	if got := hex.EncodeToString(dk); got != want {
		t.Fatalf("HKDF 派生 key = %s, want %s", got, want)
	}
	// salt 带 app_id ⇒ 换项目就是另一把 key，跨项目冒用没有可乘之机
	other, err := deriveKey(key, "app2")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(other) == want {
		t.Fatal("不同 app_id 派生出同一把 key，salt 没起作用")
	}
}

func TestSealVector(t *testing.T) {
	iv, err := hex.DecodeString(vecIvHex)
	if err != nil {
		t.Fatal(err)
	}
	kid, ivB64, ctB64, err := SealWithIv(vecKey(t), iv, vecPayload())
	if err != nil {
		t.Fatal(err)
	}
	if kid != "630dcd29" || ivB64 != "q6urq6urq6urq6ur" {
		t.Fatalf("kid/iv = %s/%s, want 630dcd29/q6urq6urq6urq6ur", kid, ivB64)
	}
	if ctB64 != vecCtB64 {
		t.Fatalf("密文漂移:\n got %s\nwant %s", ctB64, vecCtB64)
	}
}

func TestOpenRoundTripAndRejections(t *testing.T) {
	key := vecKey(t)
	iv, _ := hex.DecodeString(vecIvHex)
	kid, ivB64, ctB64, err := SealWithIv(key, iv, vecPayload())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("self decrypt", func(t *testing.T) {
		p, err := Open(key, kid, "app1", ivB64, ctB64)
		if err != nil {
			t.Fatalf("自解自不通: %v", err)
		}
		if p != vecPayload() {
			t.Fatalf("解回内容 = %+v", p)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		other, _ := ParseKey(strings.Repeat("11", 32))
		if _, err := Open(other, kid, "app1", ivB64, ctB64); err == nil {
			t.Fatal("换 key 还能解开")
		}
	})
	t.Run("tampered ciphertext", func(t *testing.T) {
		bs, err := base64.StdEncoding.DecodeString(ctB64)
		if err != nil {
			t.Fatal(err)
		}
		bs[len(bs)/2] ^= 0x01 // 改中间一个比特：明文变了，tag 却没变
		tampered := base64.StdEncoding.EncodeToString(bs)
		if _, err := Open(key, kid, "app1", ivB64, tampered); err == nil {
			t.Fatal("密文被改过仍能解开（tag 校验没生效）")
		}
	})
	t.Run("other app cannot reuse", func(t *testing.T) {
		// 表头换成 app2：AAD 与派生 salt 双双变化，必须直接解不开
		if _, err := Open(key, kid, "app2", ivB64, ctB64); err == nil {
			t.Fatal("app_id 参与了加密，却仍能解开")
		}
	})
	t.Run("other kid cannot reuse", func(t *testing.T) {
		if _, err := Open(key, "ffffffff", "app1", ivB64, ctB64); err == nil {
			t.Fatal("kid 参与了 AAD，却仍能解开")
		}
	})
	t.Run("malformed base64", func(t *testing.T) {
		if _, err := Open(key, kid, "app1", "!!!", ctB64); err == nil {
			t.Fatal("畸形 iv 应报错")
		}
		if _, err := Open(key, kid, "app1", ivB64, "!!!"); err == nil {
			t.Fatal("畸形密文应报错")
		}
	})
}

// 每次 Seal 都用新随机 iv ⇒ 同一口令注册两次的密文必然不同（这是"抓到包也不能重用/比对"的前提）。
func TestSealIsNonDeterministic(t *testing.T) {
	key := vecKey(t)
	_, iv1, ct1, err := Seal(key, vecPayload())
	if err != nil {
		t.Fatal(err)
	}
	_, iv2, ct2, err := Seal(key, vecPayload())
	if err != nil {
		t.Fatal(err)
	}
	if iv1 == iv2 || ct1 == ct2 {
		t.Fatalf("两次 Seal 得到同样的 iv/密文，iv 没随机:\n%s/%s\n%s/%s", iv1, ct1, iv2, ct2)
	}
}

func TestParseKeyRejects(t *testing.T) {
	for _, bad := range []string{"", "zz", strings.Repeat("00", 16), strings.Repeat("00", 31),
		strings.Repeat("00", 33), "0x" + strings.Repeat("00", 31)} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) 应当报错", firstRunes(bad, 8))
		}
	}
	if _, err := ParseKey(strings.ToUpper(vecKeyHex)); err != nil {
		t.Errorf("大写 hex 应当接受: %v", err)
	}
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
