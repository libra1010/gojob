package com.gojob.worker.spring;

import java.util.ArrayList;
import java.util.List;
import org.springframework.boot.context.properties.ConfigurationProperties;

/**
 * gojob worker 的 Spring Boot 配置项（前缀 {@code gojob.worker}）。
 *
 * <p>字段与 core 模块 {@link com.gojob.worker.OpenConfig} 一一对应；
 * 未显式配置的项沿用 OpenConfig 默认值。
 */
@ConfigurationProperties(prefix = "gojob.worker")
public class GoJobWorkerProperties {

    /** 总开关：false 时完全不装配 worker。 */
    private boolean enable = true;

    /**
     * Server 数据面地址，如 {@code http://127.0.0.1:7700}；多副本可用逗号一次写全
     * （{@code gojob.worker.server-address=http://h1:7700,http://h2:7700}）。enable=true 时必填。
     */
    private String serverAddress;

    /**
     * 集群各副本地址（{@code gojob.worker.server-addresses[0]=...}）。与 {@link #serverAddress}
     * 合并去重；填了就不需要给 worker 单独挂 LB/VIP，SDK 自身寻址容错并跟随调度归属副本。
     */
    private List<String> serverAddresses = new ArrayList<>();

    /** 应用 ID，需已在 Server 创建（/openapi/app/create）。enable=true 时必填。 */
    private String appId;

    /** 应用密码。 */
    private String password = "";

    /** 本节点对外标识（worker instance 地址）；不配则默认 hostname-pid。 */
    private String workerInstanceId;

    /** 同时执行的任务上限（执行线程池大小）。 */
    private int maxConcurrency = 16;

    /** 心跳间隔秒数。 */
    private int heartBeatSeconds = 5;

    /** 长轮询单次最大阻塞秒数（服务端上限 120）。 */
    private int fetchWaitSeconds = 30;

    /** SDK 类型标识（心跳上报统计页）；SpringBoot 环境默认 JAVA_SPRINGBOOT，可覆盖。 */
    private String sdkType = "JAVA_SPRINGBOOT";

    /** 机器标签，供 SPECIFY 分发策略匹配（gojob.worker.tag）。 */
    private String tag = "";

    public boolean isEnable() {
        return enable;
    }

    public void setEnable(boolean enable) {
        this.enable = enable;
    }

    public String getServerAddress() {
        return serverAddress;
    }

    public void setServerAddress(String serverAddress) {
        this.serverAddress = serverAddress;
    }

    public List<String> getServerAddresses() {
        return serverAddresses;
    }

    public void setServerAddresses(List<String> serverAddresses) {
        this.serverAddresses = serverAddresses;
    }

    public String getAppId() {
        return appId;
    }

    public void setAppId(String appId) {
        this.appId = appId;
    }

    public String getPassword() {
        return password;
    }

    public void setPassword(String password) {
        this.password = password;
    }

    public String getWorkerInstanceId() {
        return workerInstanceId;
    }

    public void setWorkerInstanceId(String workerInstanceId) {
        this.workerInstanceId = workerInstanceId;
    }

    public int getMaxConcurrency() {
        return maxConcurrency;
    }

    public void setMaxConcurrency(int maxConcurrency) {
        this.maxConcurrency = maxConcurrency;
    }

    public int getHeartBeatSeconds() {
        return heartBeatSeconds;
    }

    public void setHeartBeatSeconds(int heartBeatSeconds) {
        this.heartBeatSeconds = heartBeatSeconds;
    }

    public int getFetchWaitSeconds() {
        return fetchWaitSeconds;
    }

    public void setFetchWaitSeconds(int fetchWaitSeconds) {
        this.fetchWaitSeconds = fetchWaitSeconds;
    }

    public String getSdkType() {
        return sdkType;
    }

    public void setSdkType(String sdkType) {
        this.sdkType = sdkType;
    }

    public String getTag() {
        return tag;
    }

    public void setTag(String tag) {
        this.tag = tag;
    }

    /** 转成 core 的 {@link com.gojob.worker.OpenConfig}。 */
    public com.gojob.worker.OpenConfig toOpenConfig() {
        com.gojob.worker.OpenConfig config = new com.gojob.worker.OpenConfig()
                .setServerAddress(serverAddress)
                .setServerAddresses(serverAddresses)
                .setAppId(appId)
                .setPassword(password)
                .setMaxConcurrency(maxConcurrency)
                .setHeartBeatSeconds(heartBeatSeconds)
                .setFetchWaitSeconds(fetchWaitSeconds)
                .setSdkType(sdkType)
                .setTag(tag);
        if (workerInstanceId != null && !workerInstanceId.isEmpty()) {
            config.setAddress(workerInstanceId);
        }
        return config;
    }
}
