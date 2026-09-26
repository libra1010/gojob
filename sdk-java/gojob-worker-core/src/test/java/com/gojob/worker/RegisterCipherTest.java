// Java SDK 的注册口令加密：字节口径必须与服务端/Go SDK 完全一致，否则解不开的就是自己。
//
// 这里的期望值与 internal/registercrypto 的 vector 测试同源，但那组数是**用 Node 的
// OpenSSL 实现独立算出来的**（三方各自对齐一个外部参照，而不是"两边自己说一致"）。
// JCE 没有 HKDF，deriveKey 是照 RFC 5869 手写的，所以先用 RFC 的官方测试向量钉住它。
package com.gojob.worker;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.gojob.worker.internal.RegisterCipher;
import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.HexFormat;
import javax.crypto.Cipher;
import javax.crypto.spec.GCMParameterSpec;
import javax.crypto.spec.SecretKeySpec;
import org.junit.jupiter.api.Test;

class RegisterCipherTest {

    private static final String KEY_HEX =
            "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f";
    private static final String APP_ID = "app1";

    // 与 Go 侧 vector_test.go 同一组常量
    private static final String GO_PLAIN_JSON =
            "{\"v\":\"gojob-register:v1\",\"app_id\":\"app1\",\"instance_id\":\"i1\","
                    + "\"challenge\":\"ch1\",\"password\":\"pw1\"}";
    private static final String GO_IV_B64 = "q6urq6urq6urq6ur";
    private static final String GO_CT_B64 =
            "3oespKc2ZiWBeiCY1B+kJEyY8uEDyJrJCpstWAHbXZCgSWA8gTJeFnY3nF/197Hee7F3"
                    + "jDFVfJ7s16BbheF80UgazSreTNscs3AUX2Wpt8unfyOkraO1Oglmt9RBXg0Gzb1IEiyTZFWJKbOqANyo";

    /** RFC 5869 test case 1（HMAC-SHA256）：手写 HKDF 的第一道关。 */
    @Test
    void hkdfMatchesRfc5869() {
        byte[] ikm = HexFormat.of().parseHex("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b");
        byte[] salt = HexFormat.of().parseHex("000102030405060708090a0b");
        byte[] info = HexFormat.of().parseHex("f0f1f2f3f4f5f6f7f8f9");
        byte[] okm = hkdf(ikm, salt, info, 42);
        assertEquals("0b4c947951f674e4b25d40cf50818799106e6b68e75e09e069578fa4615e12bf3f1751895b64ba52e408",
                HexFormat.of().formatHex(okm));
    }

    @Test
    void derivedKeyAndKidMatchTheSharedVector() {
        byte[] key = RegisterCipher.parseKey(KEY_HEX);
        assertEquals("630dcd29", RegisterCipher.keyId(key));
        assertEquals("a5c986d6b858a46df5db12820b41522e8c8c12039ec51b9c96762398ae4c8a81",
                HexFormat.of().formatHex(RegisterCipher.deriveKey(key, APP_ID)));
        // salt 带 app_id ⇒ 换项目就是另一把 key
        assertNotEquals(HexFormat.of().formatHex(RegisterCipher.deriveKey(key, "app2")),
                HexFormat.of().formatHex(RegisterCipher.deriveKey(key, APP_ID)));
    }

    /** 直接解 Go 侧产出的密文：跨语言互操作不靠"两边各自测过"，靠这一条。 */
    @Test
    void javaDecryptsGoCiphertext() throws Exception {
        byte[] plain = decrypt(RegisterCipher.deriveKey(RegisterCipher.parseKey(KEY_HEX), APP_ID),
                Base64.getDecoder().decode(GO_IV_B64), Base64.getDecoder().decode(GO_CT_B64),
                aad(RegisterCipher.keyId(RegisterCipher.parseKey(KEY_HEX)), APP_ID));
        assertEquals(GO_PLAIN_JSON, new String(plain, StandardCharsets.UTF_8));
    }

    @Test
    void sealProducesWhatTheServerFormatExpects() throws Exception {
        byte[] key = RegisterCipher.parseKey(KEY_HEX);
        ObjectMapper mapper = new ObjectMapper();
        RegisterCipher.Sealed a = RegisterCipher.seal(key, APP_ID, "i1", "ch-1", "pw1", mapper);
        RegisterCipher.Sealed b = RegisterCipher.seal(key, APP_ID, "i1", "ch-2", "pw1", mapper);

        assertEquals(RegisterCipher.keyId(key), a.kid());
        assertEquals("ch-1", a.challenge());
        byte[] iv = Base64.getDecoder().decode(a.iv());
        assertEquals(12, iv.length, "GCM nonce 必须是 12 字节");
        // 同一口令两次 seal 的密文必须不同（iv 随机）——否则密文就成了口令的替身
        assertNotEquals(a.iv(), b.iv());
        assertNotEquals(a.ciphertext(), b.ciphertext());

        byte[] plain = decrypt(RegisterCipher.deriveKey(key, APP_ID), iv,
                Base64.getDecoder().decode(a.ciphertext()), aad(a.kid(), APP_ID));
        String json = new String(plain, StandardCharsets.UTF_8);
        assertTrue(json.contains("\"password\":\"pw1\""), json);
        assertTrue(json.contains("\"challenge\":\"ch-1\""), json);
        assertTrue(json.contains("\"v\":\"gojob-register:v1\""), json);
        assertFalse(json.isEmpty());
    }

    /** AAD 绑了 kid 与 app_id：换任一个都解不开（冒用没有可乘之机）。 */
    @Test
    void aadBindsKidAndApp() throws Exception {
        byte[] key = RegisterCipher.parseKey(KEY_HEX);
        RegisterCipher.Sealed s = RegisterCipher.seal(key, APP_ID, "i1", "ch-1", "pw1", new ObjectMapper());
        byte[] dk = RegisterCipher.deriveKey(key, APP_ID);
        byte[] iv = Base64.getDecoder().decode(s.iv());
        byte[] ct = Base64.getDecoder().decode(s.ciphertext());
        assertThrows(javax.crypto.AEADBadTagException.class,
                () -> decrypt(dk, iv, ct, aad("ffffffff", APP_ID)), "换 kid 应 tag 校验失败");
        assertThrows(javax.crypto.AEADBadTagException.class,
                () -> decrypt(RegisterCipher.deriveKey(key, "app2"), iv, ct, aad(s.kid(), "app2")),
                "换 app 应 tag 校验失败");
    }

    @Test
    void parseKeyRejectsWrongShapes() {
        assertThrows(IllegalArgumentException.class, () -> RegisterCipher.parseKey(null));
        assertThrows(IllegalArgumentException.class, () -> RegisterCipher.parseKey(""));
        assertThrows(IllegalArgumentException.class, () -> RegisterCipher.parseKey("zz" + KEY_HEX.substring(2)));
        assertThrows(IllegalArgumentException.class, () -> RegisterCipher.parseKey(KEY_HEX.substring(0, 62)));
        // 大小写都认（hex 无所谓大小写，卡这个只会添麻烦）
        assertArrayEquals(RegisterCipher.parseKey(KEY_HEX),
                RegisterCipher.parseKey(KEY_HEX.toUpperCase()));
    }

    private static byte[] aad(String kid, String appId) {
        return (RegisterCipher.PAYLOAD_VERSION + "\n" + kid + "\n" + appId).getBytes(StandardCharsets.UTF_8);
    }

    /** 测试专用解密：走 JCE 的 GCM，与生产代码的 seal 互为反向，不复用它的内部状态。 */
    private static byte[] decrypt(byte[] aesKey, byte[] iv, byte[] ct, byte[] aad) throws Exception {
        Cipher c = Cipher.getInstance("AES/GCM/NoPadding");
        c.init(Cipher.DECRYPT_MODE, new SecretKeySpec(aesKey, "AES"), new GCMParameterSpec(128, iv));
        c.updateAAD(aad);
        return c.doFinal(ct);
    }

    /** 独立实现一遍 HKDF，只为对照 RegisterCipher.deriveKey（RFC 5869 第 2.2 节）。 */
    private static byte[] hkdf(byte[] ikm, byte[] salt, byte[] info, int length) {
        byte[] prk = hmac(salt, ikm);
        byte[] okm = new byte[length];
        byte[] prev = new byte[0];
        int off = 0;
        for (byte i = 1; off < length; i++) {
            prev = hmac(prk, concat(prev, info, new byte[] {i}));
            int n = Math.min(prev.length, length - off);
            System.arraycopy(prev, 0, okm, off, n);
            off += n;
        }
        return okm;
    }

    private static byte[] hmac(byte[] key, byte[] data) {
        try {
            javax.crypto.Mac mac = javax.crypto.Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(key, "HmacSHA256"));
            return mac.doFinal(data);
        } catch (Exception e) {
            throw new IllegalStateException(e);
        }
    }

    private static byte[] concat(byte[]... parts) {
        int len = 0;
        for (byte[] p : parts) {
            len += p.length;
        }
        byte[] out = new byte[len];
        int off = 0;
        for (byte[] p : parts) {
            System.arraycopy(p, 0, out, off, p.length);
            off += p.length;
        }
        return out;
    }
}
