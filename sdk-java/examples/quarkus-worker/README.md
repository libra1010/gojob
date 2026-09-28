# quarkus-worker (example)

gojob Java Worker SDK 的 Quarkus 用法示例。

本模块使用真实 Quarkus 依赖（`io.quarkus:quarkus-arc` + `io.quarkus:quarkus-core`，
版本由父 POM `dependencyManagement` 管理，Quarkus 3.15.x），编译级演示：

- `GoJobWorkerLifecycle`：`@Observes StartupEvent` 时创建并 `start()` 一个
  `GoJobWorker`；`@Observes ShutdownEvent` 时调用 `close()` 优雅下线
  （停止收单 → 等运行中任务执行并回报完毕 → 释放资源）；
- `HelloProcessor`：`@ApplicationScoped` 的 `Processor` bean，内部正常注入业务
  bean（`GreetingService`）。注册 key 用**类全名**
  `com.gojob.example.quarkus.HelloProcessor`，与 Server 端 Job 的
  `processor` 字段（含 PowerJob 导入任务的 `processorInfo`）语义一致。

配置通过 MicroProfile Config 读取（见 `src/main/resources/application.properties`，
生产环境用环境变量 `GOJOB_SERVER_ADDRESS` / `GOJOB_APP_ID` / `GOJOB_PASSWORD` 覆盖）。

## 在真实 Quarkus 工程中使用

示例仅演示接线方式（`mvn package` 只编译打包，不做 Quarkus augmentation）。
在真实应用中：

1. 引入 `io.quarkus:quarkus-arc`（Quarkus BOM 管理版本）+ `com.gojob:gojob-worker-core`；
2. 复制 `GoJobWorkerLifecycle` / `HelloProcessor` 的写法；
3. 用 `quarkus-maven-plugin` 打包后 `quarkus:dev` / `java -jar target/quarkus-app/quarkus-run.jar` 运行。
