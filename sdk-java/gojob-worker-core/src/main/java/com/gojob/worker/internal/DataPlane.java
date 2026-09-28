package com.gojob.worker.internal;

import com.fasterxml.jackson.databind.JsonNode;
import com.gojob.worker.dto.Tokenized;
import java.io.IOException;

/**
 * 带 worker token 的数据面调用入口，由 {@code GoJobWorker} 提供。
 *
 * <p>实现方负责填 token，并在服务端返回 401（token 过期/服务重启）时重新注册后重放一次，
 * 因此日志通道等业务组件不必各自处理凭证轮换。
 */
@FunctionalInterface
public interface DataPlane {

    JsonNode call(String path, Tokenized request) throws IOException;
}
