# API Reference

先读[客户端接入与使用（中文）](client-integration.zh-CN.md)，确认目标服务的部署能力与权限。
OpenAI 兼容推理使用 origin + `/v1`，管理/有限控制使用 origin + `/api/v1`。

## 定制接口契约

- [有限 Router 控制 p3a/1 与可选 N1 扩展](control-api.zh-CN.md)：状态、有限操作、就绪身份、恢复、持久评测/recipe/service-use。
- [control-openapi.yaml](control-openapi.yaml)：上述扩展的机读权威；其中推理 header 约束描述 N1 模式，不代表普通上游请求一律要求 use ID。
- [资源观测](resources.md)、[owner 暂停与释放](resource-policy.md)：独立鉴权与资源口径；不能从推理成功推定已释放。
- [单配置固定合成测试](deployment-tests.md)、[N1 客户端/评测工具](evaluator-client.md)：两种测试协议，不混用保留/重试规则。

扩展的源码或 schema 存在，不等于具体环境已部署、已授权或业务质量通过；403 也不等于未部署。
不要为扩展重新生成或覆盖上游 Swagger，亦不把所有原生后端路径混入有限控制契约。

<a id="upstream-api"></a>

## Upstream API

以下为上游 Swagger；未包含全部定制契约，受控实例的准入/权限限制以上方扩展文档为准。

[OAD(swagger.yaml)]