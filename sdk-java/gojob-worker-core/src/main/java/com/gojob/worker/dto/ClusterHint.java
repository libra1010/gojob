package com.gojob.worker.dto;

import com.fasterxml.jackson.annotation.JsonProperty;
import java.util.List;

/**
 * 心跳响应里夹带的集群路由提示，对应 {@code protocol.ClusterHint}。
 *
 * <p>{@code owner} 只是<b>首选地址提示</b>，不是唯一通路：给错、给晚、甚至完全拿不到，
 * 最坏代价只是一次跨副本信号，不会让任务失败。这一点与兼容协议的
 * {@code app_info.current_server} 不同——那边 worker 只能从 owner 领任务，
 * 所以它必须每 10s 额外问一次 {@code /server/acquire}、对 owner 发 PING 判活，
 * 并且连续 3 次找不到 owner 就本地杀死秒级任务。本 SDK 一个都不需要。
 */
public class ClusterHint {

    /** 集群内在线副本对 worker 可达的地址；SDK 并进已知地址集合（新副本无需重启 worker 即可被发现）。 */
    @JsonProperty("servers")
    public List<String> servers;

    /** 本 app 调度归属副本的可达地址；空 = 没有可用提示。 */
    @JsonProperty("owner")
    public String owner;

    /** 归属副本的 node_id，只用于日志与排障，不参与路由判断。 */
    @JsonProperty("owner_node")
    public String ownerNode;
}
