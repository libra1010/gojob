package com.gojob.worker.internal;

import com.fasterxml.jackson.databind.ObjectMapper;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.Map;
import javax.crypto.Cipher;
import javax.crypto.Mac;
import javax.crypto.spec.GCMParameterSpec;
import javax.crypto.spec.SecretKeySpec;

/**
 * 注册口令加密（一次性 challenge + AES-256-GCM）的 Java 侧实现。
 *
 * <p>字节口径与服务端 {@code internal/registercrypto} 完全一致，且双方各自对齐同一组
 * 外部参照（见 registercipher 的 vector 测试与 {@link #deriveKey} 上的 RFC 5869 断言）：
 * <ul>
 *   <li>key 派生：HKDF-SHA256(ikm=配置的 AES key, salt="gojob-register:v1|" + appId,
 *       info="gojob-worker-register", L=32)。JCE 没有 HKDF，所以这里手写——它只有
 *       extract/expand 两步，逐字照 RFC 5869。</li>
 *   <li>明文：payload 的 JSON（字段顺序与 Go 侧一致，纯省事：两边能互相核对期望密文）</li>
 *   <li>密文：AES/GCM/NoPadding，12 字节随机 iv <b>单独传</b>、认证标签附在密文尾部
 *       ——这正是 JCE 的默认输出形态，与 Go 的 {@code gcm.Seal(nil, iv, plain, aad)} 同构</li>
 *   <li>AAD："gojob-register:v1\n" + kid + "\n" + appId</li>
 *   <li>kid：sha256(key) 前 4 字节的 hex</li>
 *   <li>iv / 密文上线时各自标准 base64（带填充）</li>
 * </ul>
 *
 * <p>为什么CBC不行：CBC 没有完整性，解出垃圾时分不清"key 错/被改过/口令错"，
 * 把这几类返成不同文案就是 padding oracle 的入口。GCM 的 tag 校验失败只有一个结局。
 */
public final class RegisterCipher {

    /** 载荷版本，同时参与 HKDF salt 与 AAD；换算法/换口径时开新值，别让旧客户端静默失败。 */
    public static final String PAYLOAD_VERSION = "gojob-register:v1";

    public static final String SCHEME_PLAIN = "plain";
    public static final String SCHEME_AES_GCM = "aes-gcm";

    private static final byte[] HKDF_INFO = "gojob-worker-register".getBytes(StandardCharsets.UTF_8);
    private static final int IV_LENGTH = 12;
    private static final int TAG_BITS = 128;

    private RegisterCipher() {
    }

    /** 一次握手的产物，字段名与 protocol.RegisterReq 上的对应字段一致。 */
    public record Sealed(String kid, String iv, String ciphertext, String challenge) {
    }

    /** 解析配置里的 key：64 位 hex（AES-256）。长度不对就是另一套算法，不猜意图。 */
    public static byte[] parseKey(String hex) {
        if (hex == null) {
            throw new IllegalArgumentException("AES key must be 64 hex chars (32 bytes, AES-256), got null");
        }
        String s = hex.trim();
        if (s.length() != 64) {
            throw new IllegalArgumentException(
                    "AES key must be 64 hex chars (32 bytes, AES-256), got " + s.length());
        }
        byte[] raw = new byte[32];
        for (int i = 0; i < 32; i++) {
            int hi = Character.digit(s.charAt(i * 2), 16);
            int lo = Character.digit(s.charAt(i * 2 + 1), 16);
            if (hi < 0 || lo < 0) {
                throw new IllegalArgumentException("AES key must be hex, got \"" + prefix(s, 8) + "...\"");
            }
            raw[i] = (byte) ((hi << 4) | lo);
        }
        return raw;
    }

    /** sha256(key) 前 4 字节 hex。kid 一律派生、不许手写。 */
    public static String keyId(byte[] key) {
        byte[] sum = sha256(key);
        StringBuilder sb = new StringBuilder(8);
        for (int i = 0; i < 4; i++) {
            sb.append(Character.forDigit((sum[i] >> 4) & 0xF, 16)).append(Character.forDigit(sum[i] & 0xF, 16));
        }
        return sb.toString();
    }

    /** HKDF-SHA256，L=32；salt 带 app_id ⇒ 同一把全局 key 在不同项目下派生出不同 key。 */
    public static byte[] deriveKey(byte[] key, String appId) {
        byte[] salt = (PAYLOAD_VERSION + "|" + appId).getBytes(StandardCharsets.UTF_8);
        byte[] prk = hmac(salt, key); // extract：PRK = HMAC-Hash(salt, IKM)，salt 是 HMAC 的密钥
        byte[] okm = new byte[32];
        byte[] prev = new byte[0];
        int off = 0;
        byte counter = 1;
        while (off < okm.length) { // expand：T(i) = HMAC(PRK, T(i-1) | info | i)
            byte[] input = concat(prev, HKDF_INFO, new byte[] {counter});
            prev = hmac(prk, input);
            int n = Math.min(prev.length, okm.length - off);
            System.arraycopy(prev, 0, okm, off, n);
            off += n;
            counter++;
        }
        return okm;
    }

    /**
     * 加密一份注册载荷。iv 每次全新随机 ⇒ 同一口令注册两次的密文必然不同，
     * 抓包者既比不出"这两次是同一个节点"，也没法拿旧包重放（challenge 一次性）。
     */
    public static Sealed seal(byte[] key, String appId, String instanceId, String challenge,
            String password, ObjectMapper mapper) {
        String kid = keyId(key);
        try {
            Map<String, String> payload = new LinkedHashMap<>();
            payload.put("v", PAYLOAD_VERSION);
            payload.put("app_id", appId);
            payload.put("instance_id", instanceId);
            payload.put("challenge", challenge);
            payload.put("password", password);
            byte[] plain = mapper.writeValueAsString(payload).getBytes(StandardCharsets.UTF_8);
            byte[] iv = new byte[IV_LENGTH];
            java.security.SecureRandom.getInstanceStrong().nextBytes(iv);
            Cipher cipher = Cipher.getInstance("AES/GCM/NoPadding");
            cipher.init(Cipher.ENCRYPT_MODE, new SecretKeySpec(deriveKey(key, appId), "AES"),
                    new GCMParameterSpec(TAG_BITS, iv));
            cipher.updateAAD(aad(kid, appId));
            byte[] ct = cipher.doFinal(plain); // JCE 把 tag 附在密文尾部，与 Go 侧同构
            Base64.Encoder b64 = Base64.getEncoder();
            return new Sealed(kid, b64.encodeToString(iv), b64.encodeToString(ct), challenge);
        } catch (Exception e) {
            // 不外泄细节：这里的异常都不该带出口令或 key 的内容
            throw new IllegalStateException("register payload encryption failed: " + e.getClass().getSimpleName(), e);
        }
    }

    private static byte[] aad(String kid, String appId) {
        return (PAYLOAD_VERSION + "\n" + kid + "\n" + appId).getBytes(StandardCharsets.UTF_8);
    }

    private static byte[] sha256(byte[] in) {
        try {
            return MessageDigest.getInstance("SHA-256").digest(in);
        } catch (Exception e) {
            throw new IllegalStateException(e);
        }
    }

    private static byte[] hmac(byte[] key, byte[] data) {
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
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

    private static String prefix(String s, int n) {
        return s.length() <= n ? s : s.substring(0, n);
    }
}
