package com.gojob.worker.internal;

import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.io.IOException;
import java.net.SocketException;
import java.net.SocketTimeoutException;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Collection;
import java.util.List;
import java.util.function.Consumer;
import okhttp3.Call;
import okhttp3.MediaType;
import okhttp3.OkHttpClient;
import okhttp3.Request;
import okhttp3.RequestBody;
import okhttp3.Response;
import okhttp3.ResponseBody;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * gojob 数据面 HTTP+JSON 传输：**一个已知副本地址集合 + 当前首选地址**，POST {server}{path}，
 * 解析统一响应信封 {code,message,data}。
 *
 * <p>地址有三个来源：{@code OpenConfig} 配的列表、心跳下发的 {@code cluster_hint.servers}、
 * 以及 {@code cluster_hint.owner}。任何一次"这台没答上话"（连不上、连接被reset、5xx）都会在
 * 其余已知地址上重放，所以多副本部署不再强制要求 LB/VIP——worker 手里只要还剩一个活着的副本
 * 就照常收单、回报、上报日志。
 *
 * <p>读超时对长轮询和短请求分开设置：被 {@code SIGSTOP} 冻结（长 GC、虚机挂起）的副本不会断开
 * TCP，请求只是不返回，**只有超时能认出它**。所以心跳这类短请求必须用明显更短的超时，
 * 否则一次冻结会让本节点追不到新 owner。
 */
public final class HttpTransport implements AutoCloseable {

    private static final Logger log = LoggerFactory.getLogger(HttpTransport.class);
    private static final MediaType JSON = MediaType.parse("application/json; charset=utf-8");
    /** 只有这一个路径是长阻塞的，其余请求都按短超时走。 */
    private static final String LONG_POLL_PATH = "/worker/fetchTask";

    private final Object lock = new Object();
    private final List<String> known = new ArrayList<>();
    private String current;

    private final OkHttpClient shortCallClient;
    private final OkHttpClient longPollClient;
    private final ObjectMapper mapper;

    /** 首选地址因为"那台没答上话"而变更后回调（不带锁调用）；worker 用它掐断挂在旧地址上的长轮询。 */
    private Runnable rotateHook;

    /**
     * @param serverAddresses        已知副本地址（有序，首个即启动首选地址）
     * @param readTimeoutSeconds     长轮询读超时，必须严格大于 fetch_wait_seconds，否则空轮询会被本地掐断
     * @param shortTimeoutSeconds    心跳/注册/回报/日志的读超时
     */
    public HttpTransport(List<String> serverAddresses, int readTimeoutSeconds, int shortTimeoutSeconds) {
        this.mapper = new ObjectMapper().disable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES);
        this.shortCallClient = new OkHttpClient.Builder()
                .connectTimeout(Duration.ofSeconds(10))
                .writeTimeout(Duration.ofSeconds(10))
                .readTimeout(Duration.ofSeconds(shortTimeoutSeconds))
                .build();
        // newBuilder 共享连接池与线程池，只是换一组超时，多一个客户端不额外占资源
        this.longPollClient = shortCallClient.newBuilder()
                .readTimeout(Duration.ofSeconds(readTimeoutSeconds))
                .build();
        note(serverAddresses);
        if (!known.isEmpty()) {
            current = known.get(0);
        }
    }

    public void onRotate(Runnable hook) {
        this.rotateHook = hook;
    }

    public ObjectMapper mapper() {
        return mapper;
    }

    /** 当前首选地址（长轮询与心跳都发往它）。 */
    public String current() {
        synchronized (lock) {
            return current;
        }
    }

    /** 去首尾空白与结尾斜杠；known 里存的都是这个形态，比较地址前必须先归一化。 */
    public static String normalizeAddress(String raw) {
        if (raw == null) {
            return "";
        }
        String s = raw.trim();
        while (s.endsWith("/")) {
            s = s.substring(0, s.length() - 1);
        }
        return s;
    }

    /** 把地址并进已知集合（去重、去尾斜杠），返回新增个数。 */
    public int note(Collection<String> addresses) {
        if (addresses == null) {
            return 0;
        }
        int added = 0;
        synchronized (lock) {
            for (String item : addresses) {
                String s = normalizeAddress(item);
                if (s.isEmpty() || known.contains(s)) {
                    continue;
                }
                known.add(s);
                added++;
            }
        }
        return added;
    }

    /** 直接把首选地址设为 {@code addr}（owner 提示用）；它不在已知集合里则视为无效跳转。 */
    public boolean jumpTo(String addr) {
        String target = normalizeAddress(addr);
        synchronized (lock) {
            if (target.isEmpty() || !known.contains(target)) {
                return false;
            }
            current = target;
            return true;
        }
    }

    /**
     * 把首选地址换成"失败那台的下一台"（循环）并返回它；只有一个地址时原地返回。
     *
     * <p>必须按 {@code failed} 而不是按当前地址来挪：心跳与长轮询共用这一个首选地址，
     * 谁失败就从当前地址往后挪会在 A→B→A 之间来回弹。失败者已不在集合里则不动当前地址。
     */
    public String rotateFrom(String failed) {
        synchronized (lock) {
            if (known.size() > 1) {
                int i = known.indexOf(failed);
                if (i >= 0) {
                    current = known.get((i + 1) % known.size());
                } else if (failed == null || failed.equals(current)) {
                    current = known.get(0);
                }
            }
            return current;
        }
    }

    /**
     * 发一次请求；"这个副本没答上话"就在其余已知地址上重放，最多把已知集合走一遍。
     *
     * @param onCall 请求已构造、尚未执行时的回调；fetch 用它记住 Call 以便 {@code close()} 或地址变更时掐断
     * @throws ServerException 信封 {@code code != 0}（含 401，由上层重注册后重放）
     */
    public JsonNode request(String path, Object requestBody, Consumer<Call> onCall) throws IOException {
        int attempts;
        synchronized (lock) {
            attempts = known.size();
        }
        if (attempts == 0) {
            throw new IOException("no server address configured");
        }
        IOException last = null;
        for (int i = 0; i < attempts; i++) {
            String base = current();
            try {
                Call call = newCall(path, requestBody, base);
                if (onCall != null) {
                    onCall.accept(call);
                }
                return execute(call);
            } catch (ServerException e) {
                throw e; // 业务答复，换副本结果一样；401 由上层重注册后重放
            } catch (HttpStatusException e) {
                if (e.status() < 500) {
                    throw e;
                }
                last = e;
                log.warn("gojob worker: server {} 返回 {}，换下一台", base, e.getMessage());
            } catch (SocketTimeoutException e) {
                // 整轮预算已经花光（心跳的短超时、长轮询的 wait+余量都是"这一轮"的额度）：
                // 换一个地址，但别带着已经到点的读超时立刻去撞下一台，那只会立刻再失败并留下误导日志。
                log.warn("gojob worker: server {} 本轮超时，下一轮改由 {} 服务", base, rotate(base));
                throw e;
            } catch (SocketException e) {
                // ConnectException（连不上/端口没开）也是 SocketException：换一台很可能就通
                last = e;
                log.warn("gojob worker: server {} 连不上（{}），fail over to {}", base, e.toString(), rotate(base));
            } catch (IOException e) {
                // 请求被 cancel（close 或地址变更）、线程被中断：是本机自己要退出，不换副本
                throw e;
            }
        }
        throw last == null ? new IOException("all known servers failed: " + knownSnapshot()) : last;
    }

    /** 换到失败地址的下一台，并通知持有方（掐断挂在旧地址上的长轮询）。 */
    private String rotate(String failed) {
        String next = rotateFrom(failed);
        Runnable hook = rotateHook;
        if (hook != null && !next.equals(failed)) {
            hook.run();
        }
        return next;
    }

    private List<String> knownSnapshot() {
        synchronized (lock) {
            return List.copyOf(known);
        }
    }

    /** 构造一个尚未执行的请求（发往 {@code base}）。 */
    public Call newCall(String path, Object requestBody, String base) throws IOException {
        byte[] body = mapper.writeValueAsBytes(requestBody);
        Request request = new Request.Builder()
                .url(base + path)
                .post(RequestBody.create(body, JSON))
                .build();
        OkHttpClient client = LONG_POLL_PATH.equals(path) ? longPollClient : shortCallClient;
        return client.newCall(request);
    }

    /** 同步执行并校验信封，成功返回 data 节点（可能为 MissingNode/NullNode）。 */
    public JsonNode execute(Call call) throws IOException {
        try (Response response = call.execute()) {
            ResponseBody rb = response.body();
            String text = rb == null ? "" : rb.string();
            if (response.code() != 200) {
                throw new HttpStatusException(response.code(), summarize(text));
            }
            JsonNode root;
            try {
                root = mapper.readTree(text);
            } catch (IOException e) {
                throw new IOException("cannot parse server response: " + summarize(text), e);
            }
            if (root == null) {
                throw new IOException("empty server response body");
            }
            int code = root.path("code").asInt(-1);
            String message = root.path("message").asText("");
            if (code != 0) {
                throw new ServerException(code, message);
            }
            return root.path("data");
        }
    }

    private static String summarize(String s) {
        if (s == null) {
            return "";
        }
        return s.length() <= 256 ? s : s.substring(0, 256) + "...";
    }

    @Override
    public void close() {
        // 两个 client 共享同一连接池/线程池（newBuilder），关一次即可
        longPollClient.connectionPool().evictAll();
        longPollClient.dispatcher().executorService().shutdown();
        shortCallClient.connectionPool().evictAll();
        shortCallClient.dispatcher().executorService().shutdown();
    }
}
