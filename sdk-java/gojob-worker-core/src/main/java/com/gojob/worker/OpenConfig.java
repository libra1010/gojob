package com.gojob.worker;

import java.net.InetAddress;
import java.util.ArrayList;
import java.util.List;

/**
 * Worker 接入配置（对标 gojob Go SDK 的 worker.Config）。
 *
 * <p>除 serverAddress/appId 外均有默认值；setter 支持链式调用。
 */
public class OpenConfig {

    /**
     * Server 数据面地址，如 {@code http://127.0.0.1:7700}。
     * 多副本可以用逗号一次写全：{@code http://h1:7700,http://h2:7700}（与 {@link #serverAddresses} 合并去重）。
     */
    private String serverAddress;

    /**
     * 集群各副本地址。填了就不需要再挂 LB/VIP：SDK 自己寻址与容错，
     * 并跟随服务端心跳下发的 {@code cluster_hint.owner} 把长轮询收敛到调度归属副本。
     * 与 {@link #serverAddress} 同时给会合并去重，列表首个即启动时的首选地址。
     */
    private List<String> serverAddresses = new ArrayList<>();

    /** 应用 ID，需已在 Server 创建（/openapi/app/create）。必填。 */
    private String appId;

    /** 应用密码。 */
    private String password = "";

    /** 本节点对外标识；为空时默认 hostname-pid。 */
    private String address;

    /** 同时执行的任务上限（执行线程池大小）。 */
    private int maxConcurrency = 16;

    /** 心跳间隔秒数。 */
    private int heartBeatSeconds = 5;

    /** 长轮询单次最大阻塞秒数（服务端上限 120）。 */
    private int fetchWaitSeconds = 30;

    /** SDK 类型标识（心跳上报，控制台统计页展示）：JAVA | JAVA_SPRINGBOOT | JAVA_QUARKUS。 */
    private String sdkType = "JAVA";

    /** 机器标签，供 SPECIFY 分发策略（tagIn:/tagEquals:）匹配；可逗号分隔多标签。 */
    private String tag = "";

    public String getServerAddress() {
        return serverAddress;
    }

    public OpenConfig setServerAddress(String serverAddress) {
        this.serverAddress = serverAddress;
        return this;
    }

    public List<String> getServerAddresses() {
        return serverAddresses;
    }

    public OpenConfig setServerAddresses(List<String> serverAddresses) {
        this.serverAddresses = serverAddresses;
        return this;
    }

    public String getAppId() {
        return appId;
    }

    public OpenConfig setAppId(String appId) {
        this.appId = appId;
        return this;
    }

    public String getPassword() {
        return password;
    }

    public OpenConfig setPassword(String password) {
        this.password = password;
        return this;
    }

    public String getAddress() {
        return address;
    }

    public OpenConfig setAddress(String address) {
        this.address = address;
        return this;
    }

    public int getMaxConcurrency() {
        return maxConcurrency;
    }

    public OpenConfig setMaxConcurrency(int maxConcurrency) {
        this.maxConcurrency = maxConcurrency;
        return this;
    }

    public int getHeartBeatSeconds() {
        return heartBeatSeconds;
    }

    public OpenConfig setHeartBeatSeconds(int heartBeatSeconds) {
        this.heartBeatSeconds = heartBeatSeconds;
        return this;
    }

    public int getFetchWaitSeconds() {
        return fetchWaitSeconds;
    }

    public OpenConfig setFetchWaitSeconds(int fetchWaitSeconds) {
        this.fetchWaitSeconds = fetchWaitSeconds;
        return this;
    }

    public String getSdkType() {
        return sdkType;
    }

    public OpenConfig setSdkType(String sdkType) {
        this.sdkType = sdkType;
        return this;
    }

    public String getTag() {
        return tag;
    }

    public OpenConfig setTag(String tag) {
        this.tag = tag;
        return this;
    }

    /** 校验必填项并填充默认值；由 {@link GoJobWorker} 构造函数调用。 */
    public void applyDefaults() {
        List<String> servers = normalizeServers(serverAddress, serverAddresses);
        if (servers.isEmpty()) {
            throw new IllegalArgumentException("OpenConfig: serverAddress is required");
        }
        this.serverAddresses = servers;
        this.serverAddress = servers.get(0);
        if (appId == null || appId.isEmpty()) {
            throw new IllegalArgumentException("OpenConfig: appId is required");
        }
        if (password == null) {
            password = "";
        }
        if (address == null || address.isEmpty()) {
            address = detectAddress();
        }
        if (maxConcurrency <= 0) {
            maxConcurrency = 16;
        }
        if (heartBeatSeconds <= 0) {
            heartBeatSeconds = 5;
        }
        if (fetchWaitSeconds <= 0) {
            fetchWaitSeconds = 30;
        }
        if (sdkType == null || sdkType.isEmpty()) {
            sdkType = "JAVA";
        }
        if (tag == null) {
            tag = "";
        }
    }

    /**
     * 把 {@code serverAddress}（可逗号分隔）与 {@code serverAddresses} 合成一个
     * 去重、去尾斜杠的有序列表；顺序即启动时的首选顺序。语义与 Go SDK 的 normalizeServers 一致。
     */
    static List<String> normalizeServers(String single, List<String> list) {
        List<String> out = new ArrayList<>();
        List<String> raw = new ArrayList<>();
        if (single != null) {
            raw.addAll(List.of(single.split(",")));
        }
        if (list != null) {
            raw.addAll(list);
        }
        for (String item : raw) {
            if (item == null) {
                continue;
            }
            String s = item.trim();
            while (s.endsWith("/")) {
                s = s.substring(0, s.length() - 1);
            }
            if (!s.isEmpty() && !out.contains(s)) {
                out.add(s);
            }
        }
        return out;
    }

    private static String detectAddress() {
        String host = null;
        try {
            host = InetAddress.getLocalHost().getHostName();
        } catch (Exception ignored) {
            // 拿不到 hostname 时走下面的兜底
        }
        if (host == null || host.isEmpty()) {
            host = System.getenv("HOSTNAME");
        }
        if (host == null || host.isEmpty()) {
            host = "gojob-worker";
        }
        return host + "-" + ProcessHandle.current().pid();
    }
}
