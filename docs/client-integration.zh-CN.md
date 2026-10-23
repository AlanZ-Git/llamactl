# 客户端接入与使用

这份指南面向调用服务的软件开发者，不要求阅读私人部署档案或聊天记录。
HTTP 字段与边界以[有限控制契约](control-api.zh-CN.md)和
[control-openapi.yaml](control-openapi.yaml)为准；不要把本文当成第二套 API。

新工作站的构建、独立状态/鉴权和管理员初始化见[部署指南](workstation-deployment.zh-CN.md)；
普通模型下载和实例 UI 分别见[模型管理](managing-models.md)、[实例管理](managing-instances.md)。

## 1. 先选能力层，不凭仓库标题判断部署

| 能力层 | 用途与入口 | 接入前应确认 |
|---|---|---|
| 上游通用 API | 原生实例管理 `/api/v1/...`、OpenAI 兼容 `/v1/...`；[上游 API Reference](api-reference.md#upstream-api) | 管理器版本、后端类型、实例权限与是否属于受控实例 |
| 有限控制 `p3a/1` | 一个本机 llama.cpp Router；`GET .../control`、`POST .../operations`；[详细契约](control-api.zh-CN.md) | 返回 `protocol_version=p3a/1`、所需 action/参数、准入和恢复状态 |
| 可选 N1 扩展 `n1/1` | inventory、持久评测/recipe、service-use；[N1 契约](control-api.zh-CN.md#n1)、[薄客户端](evaluator-client.md) | 环境确已部署且配置 N1、key 获准相应 capability、recipe 在本地可用 |

`n1/1` 是扩展文档版本，不是旧 control 响应的新值：同一新版服务的
`GET control.protocol_version` 仍可为 `p3a/1`。有源码、测试通过、文档有 N1 标题，
均不证明目标环境已部署、调用方已获授权或业务质量已通过。本指南不宣布任何具体机器的部署状态。

接入时由管理员提供 origin、实例名、部署版本/功能约定和已授权 key。仅做相应只读发现：

- p3a controller：`GET /api/v1/instances/{name}/control`；可辅以 `GET /v1/models`。
- N1 direct key：`GET .../inventory` 取批准 root ID 和 `boot_id/revision`，
  `GET .../recipes?published=true` 取描述目录；不要求也不借用旧 control 权限。
- `/api/v1/version` 属管理鉴权组，不能要求最小权限客户端必能访问；必要时请管理员核部署版本。
- 401 是凭证问题，403 可能是缺 capability/scope，不表示“未部署”；404 也可能是错误实例、
  未启用或隐藏他人对象。查业务码和管理员确认，不能通过发起加载来探测能力。

上游[Quick Start](quick-start.md)是管理员部署普通实例的入口，不是受控实例的日常调用步骤。
受控协议启用后，旧 UI/原生 start、PUT、Router load/unload 不能代替 operations。

<a id="client-permissions"></a>

## 2. 地址、模型身份与安全边界

管理器（manager）常驻提供鉴权、状态和路由；Router 进程、模型权重是否驻留是另外的状态。
典型使用链为“客户端 → 私有网络/获准 TLS 入口 → 目标管理器 → 模型后端”。
日常客户端直接连接目标服务，不依赖开发机、评测机或 evaluator daemon 在线；网页只是可选客户端。
不要直接连未授权的后端端口绕过管理器。

本文 origin 示例为 `http://localhost:8080`，实例名 `router`、逻辑模型 `alpha` 均为虚构。
localhost 指客户端自身；远程使用时换成管理员给定的 origin，而不是复制开发机坐标。

| 项目 | 示例/规则 |
|---|---|
| manager origin | `http://localhost:8080`，不附 `/v1`、`/api/v1`、key 或查询参数 |
| OpenAI SDK base URL | `http://localhost:8080/v1`；SDK 再拼 `/chat/completions`，不要拼出 `/v1/v1` |
| 管理/控制基址 | origin + `/api/v1`，不是 `/v1/api/v1` |
| 实例名 | `router`：管理器登记的实例，不是权重文件名 |
| p3a 模型逻辑 ID | `alpha`：管理员映射返回的 ID，不接受客户端传本地模型路径 |
| 聚合推理 model | p3a 为 `router/alpha`；N1 用 service-use 返回的 `model` 加实例前缀 |
| 逐实例推理 | `/llama-cpp/router/v1/chat/completions`，body model 不加 `router/`；管理员 proxy 不是普通 key 的替代入口 |
| N1 recipe 身份 | 明确 `recipe_id` + 整数 `version`；名称/描述不是稳定主键 |
| N1 artifact 身份 | artifact ID 是数据库本地 ID；跨库不能照抄 ID，应解析导出内容身份并本地核验 |

`GET /v1/models` 是可见模型目录，不是“已加载且已验”的列表；协议实例可在未运行时列出配置的模型。
p3a 推理应绑定 `actual_model`，而不是最近 apply 的 `target_model`。

管理员负责一次性初始化/维护：批准模型根与映射、引擎、配置/数据库迁移、恢复、权限授予。
客户端不关鉴权、不编辑 DB/模型路径/引擎、不初始化日常服务，也不默认创建长期 key。
使用已存在且最小权限的凭据：

- p3a：固定单实例 scoped-controller（只允许该实例、`can_start=false/can_evict=false`）。
  普通 inference key 不能控制实例；controller 也不能管理 key 或恢复 owner/boot。
- N1 direct：`controller=false` 的单实例 inference 权限，加该实例
  `discover/service_use/inference`。评测按需加 `evaluate`；发布、写 recipe、资源观察分别授权。
  旧 controller 不自动升级为 N1 key。
- 鉴权用 `Authorization: Bearer <KEY_FROM_SECURE_STORAGE>` 或 `X-API-Key`；
  ready token、use ID 和自造 scope header 均不代替鉴权。凭据通过安全存储注入环境/受限文件，
  不放源码、URL、命令历史、普通日志或交接文本；不向客户端分发管理 key。
- 拒绝向陌生 origin 重定向携带凭据；非 loopback 使用管理员批准的私网/TLS 路径。
  跨域浏览器还需管理员核 CORS 的 origin、允许身份请求头及可读响应头；
  当前路由默认仅 expose `Link`，不能假定 JavaScript 可读全部 N1 身份头。
  不由客户端自行放宽网络/安全配置。

<a id="p3a-workflow"></a>

## 3. p3a 日常闭环

### 3.1 读状态并确定是否真的需要操作

1. `GET /api/v1/instances/router/control`，记录 boot/revision、models、configured_parameters、
   `admission`、`recovery_required`、`active_operation`、state、代际与 token。
   读状态不启动、不实时探测子模型；目录存在、Running、ready、真实推理成功分开判断。
2. `admission=blocked`/`ai_paused` 时停止新工作，请 owner 决定何时恢复；绝不自动解除。
   recovery_required、unknown、interrupted 等交给管理员核进程归属、文件与 DB。
   `PUT .../control` 的 `verified_stopped=true` 是管理员事实确认，不是客户端的便捷“解锁”。
3. 已有准确模型与参数且 ready 时复用当前就绪身份，不要每个推理请求都 apply。
   apply 即使 patch 为空也保存配置、增加 revision，并可能停止/重启模型。
4. 参数确需改变才 apply；配置无需改变且 Router stopped 时先 start（只等 Router 健康），
   之后 load 准确模型。已运行且无模型时只 load；另一模型已加载时先协调并 unload，不能抢占。

### 3.2 提交、轮询与就绪核对

操作均用 `POST /api/v1/instances/router/operations`。下面是独立的 apply 示例，
boot/revision 必须替换为刚读取的真实值，key 为本次意图新建并持久保存：

```json
{
  "action": "apply",
  "boot_id": "<current-boot>",
  "expected_revision": 0,
  "idempotency_key": "synthetic-apply-1",
  "model": "alpha",
  "patch": {"context": 2048, "gpu_layers": 0, "flash_attention": false},
  "timeout_seconds": 300
}
```

这是类型示例，不是 GPU 推荐参数。参数范围/组合读 [有限参数](control-api.zh-CN.md#finite-parameters)。
missing 保持旧覆盖；允许清除字段的 null 清除覆盖；0/false 必须原样保留，不能用真假值过滤丢失。
start/stop 不带 model/patch；load 必带 model 且无 patch；unload 可带匹配清理目标的 model、无 patch。
每个后续意图重新读当前 revision，不能套用 apply 前的值。

202 只表示接受（或找到同幂等请求）；保存 operation `id`/Location，轮询
`GET .../operations/{id}` 到 `succeeded/failed/deadline_exceeded/interrupted`。
只有 succeeded 才能进入下一步；超时不是“什么都没做”。随后再次 GET control，核对：

- 与成功 operation 同 boot、同结果 revision；state 为 ready、running 为 true。
- `actual_model` 等于请求模型，有非空 `ready_token`，与本次就绪结果一致；记录 process/load 代际。
- 没有新的恢复/owner 屏障。operation 中旧 token 是历史结果，不能跳过当前快照。

ready 只确认最近引擎健康、准确 loaded 模型和目标 props，不是持续健康或 GPU/质量保证。

### 3.3 推理与有效输出

非流式请求（鉴权和 X-Ready-Token 均由程序从安全存储/当前快照注入）：

```http
POST /v1/chat/completions
Authorization: Bearer <KEY_FROM_SECURE_STORAGE>
X-Ready-Token: <current-ready-token>
Content-Type: application/json

{"model":"router/alpha","messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":32,"stream":false,"chat_template_kwargs":{"enable_thinking":false}}
```

对相关支持该模板参数的模型显式 `chat_template_kwargs.enable_thinking=false`，
不要把 reasoning-only 当正文；其他模型先核引擎/模板支持，不把它当通用参数。

客户端非流式检查 HTTP/JSON error、准确模型、`choices[].message.content` 的非空有效正文及
finish_reason；`length` 是截断，不能作为完整答案或质量成功。HTTP 200 本身不够。

SSE 将同一 body 的 `stream` 设 true：按 SSE 事件边界组合 `data:`，解析 JSON chunk，
只将 `choices[].delta.content` 的有效正文累积给用户；role/reasoning/tool/usage 帧不算正文。
核最终 finish_reason 和 `data: [DONE]`，异常 error、空正文、截断或断流单独记录为失败/不确定，
不能将“收到第一个帧”当完成。SDK 必须支持传本协议额外 header；不支持则换可传 header 的客户端。

关闭 UI、取消前端请求或断开 SSE 不等于 unload/stop，也不保证显存已释放。
同一次不确定生成不要自动重试，防止重复执行和重复业务副作用。

### 3.4 用后释放与切换协调

先确认本客户端的转发全部退出，再读 control，比较本次保存的 boot/revision、ready token、
process/load 代际和模型。身份变了就不自动停止：交给协调者核谁拥有当前加载。
身份未变且协调约定允许，提交 unload 或 stop 并轮询到成功；切换模型先释放，再 start/load 或 apply。
unload 只确认模型卸载，stop 才等待本代归属树退出；manager 继续常驻。
两者都不保证整卡 VRAM 瞬时归零，外部程序资源不归客户端管理。

p3a 没有跨多请求的 use reservation，也没有 stop 的 process/load CAS 字段；
即使停止前读到同身份，仍不能替代多客户端之间的外部串行协调。busy 立即拒绝、不排队；
已有 SSE 未结束时不能维护。不要通过循环 stop/apply 来“抢到”实例。

旧标准库 `examples/control_client.py`（仓库根相对路径）仅演示 p3a 的 apply→ready→一次合成推理→stop，
并不是 N1 客户端，也不含完整生产输出/质量校验或所有模型的 thinking 策略。
运行示例会真实改配置和加载；只在已授权环境使用，不作为健康探针或每请求调用器。

## 4. 可选 N1：recipe → service-use → infer → release

### 4.1 发现并选择明确版本

用最小 N1 key 读 inventory，获取 envelope 的当前 `boot_id/revision`；
再读 `recipes?published=true`，按描述、task_tags、部署参数、request contract、结论与限制选
确切 recipe_id/version，并用 `GET .../recipes/{recipe_id}/versions/{version}` 核原文。
`effective_available` 仅指本地发布/身份可用，不是 ready，也不是业务验证或推荐。

若目录为空，先问管理员是否已发布可用 recipe，不自行转成旧 apply。需要新模型时，
有相应权限的操作者才做 scans→分页/partial continuation→verify→register。
inventory GET 仅返回根 ID，不是候选全集；扫描/verify/register 都不加载，
verified 只证明内容/成员身份，不保证权重、引擎兼容或质量。详见 [N1 目录规则](control-api.zh-CN.md#n1)。

### 4.2 建立服务使用并绑定身份

`POST /api/v1/instances/router/service-uses` 使用 N1 自己的请求体（不是 action/model/patch）：

```json
{
  "boot_id": "<current-inventory-boot>",
  "expected_revision": 0,
  "idempotency_key": "synthetic-use-1",
  "recipe_id": "<selected-recipe-id>",
  "version": 1,
  "idle_seconds": 60
}
```

保存原请求及返回 `run_id`（活跃时也有 use_id）；轮询 `GET .../service-uses/{id}`。
成功就绪须有非空 ready_token、准确 recipe_id/recipe_version/model、request_contract、
prompt_digest 和 identity；如先 frozen_at 终态则读失败/清理，不进行推理。
切换/就绪事件已持久化后才开放准入。model 从 use 返回值取，不能猜 artifact ID 或用 recipe 名代替。

同一 use 所有者 key 发起 `POST /v1/chat/completions`，下表前四项身份 header 必须全部提供，且每项恰好一个值；最后一项超时 header 可选：

| header | 取值 |
|---|---|
| X-Service-Use | 本次 use 的 ID |
| X-Ready-Token | 当前 use 的 ready_token |
| X-Prompt-Template-Version | request_contract.template_version |
| X-Prompt-Digest | 当前 use 的 prompt_digest；按契约 canonical JSON 复算核验 |
| X-Request-Timeout-Seconds | 可选 1–600 秒，并受 use 绝对截止约束 |

body 示例（将 model 替换为实例前缀 + use.model）：

```json
{"model":"router/artifact-id","messages":[{"role":"user","content":"synthetic test"}]}
```

recipe 的生成参数/system_prefix/template_version 是冻结请求合同：missing 由服务端填充，
主动提供的值必须完全一致；不能临时更改 max_tokens、thinking 或 system 前缀。
prompt digest 不是任意 JSON 序列化摘要，应复用仓库 `tools/evaluator/evaluator.py` 中 Client 的 canonical/digest。
响应的 X-Boot-ID/X-Config-Revision/X-Recipe-ID/X-Recipe-Version/
X-Process-Generation/X-Load-Generation/X-Prompt-Template-Version/X-Prompt-Digest
都需匹配本次 use 身份，再做正文/finish/SSE 校验。token 过期不自动建立新 use 重放推理。

用后 `POST .../service-uses/{id}/release`，body 为当前 inventory envelope
（boot_id、expected_revision、idempotency_key），轮询 GET 至 frozen_at，独立检查 state、
cleanup_state、terminal 与 owner receipt。release HTTP 200 不是清理完成；
borrowed ready 不卸载他人加载。句间 active_requests=0 不释放 reservation，切换前需结束旧 use。
idle 默认 60、最大 300 秒；renew 可带 idle_seconds 更新空闲时长，省略/0 选 60 而非保留原值。
绝对期限最多 30 分钟，有限 renew 不延长绝对期限，终态 use 不复活。
安全 release/cancel 可容忍旧 revision，但仍需当前 boot、原 run boot、所有者和合法 envelope；跨 boot 查历史而不重放。

### 4.3 复用现成薄客户端

在仓库根运行，以下只是调用模板，不会在文档验证中连接服务。
`private-client/` 是操作者事先批准的私有工作目录；key 文件由安全存储提供，不能照示例新建假 key。
messages.json 内容为 `[{"role":"user","content":"synthetic test"}]`。

```sh
python3 tools/evaluator/evaluator.py --url http://localhost:8080 --instance router --key-file private-client/n1.key discover
python3 tools/evaluator/evaluator.py --url http://localhost:8080 --instance router --key-file private-client/n1.key use RECIPE_ID 1 --checkpoint private-client/use.json
python3 tools/evaluator/evaluator.py --url http://localhost:8080 --instance router --key-file private-client/n1.key infer USE_ID private-client/messages.json
python3 tools/evaluator/evaluator.py --url http://localhost:8080 --instance router --key-file private-client/n1.key release USE_ID
```

RECIPE_ID/version 从目录选；USE_ID 取 use 返回的 run_id，不把大写占位符原样发到真实服务。
use 会轮询；复用同 checkpoint 会查原请求，不新建 use；infer 不自动重试。
薄客户端核身份，不替代调用方正文/业务判定；输出含 token/身份/正文，禁止将终端输出原样公开。
checkpoint 是私人缓存，不是服务端事实账本。

<a id="evaluation-recipes"></a>

### 4.4 外部有界评测与导入/发布

完整 plan 形状和可运行合成例（仓库 `tools/evaluator/synthetic_http.py`）见
[工具说明](evaluator-client.md)；不在日常客户端强制引入评测服务。
plan 包含 execution（准确候选内容身份、完整 deployment/request、参数范围、fixture_version、
wall/attempt/cleanup 和 requests/tokens/bytes 预算、真实 notice_ref/not_before）及 fixtures。
`plan` 仅 prepared，不发送通知，不等于有运行授权。

```sh
python3 tools/evaluator/evaluator.py plan private-client/plan.json
python3 tools/evaluator/evaluator.py --url http://localhost:8080 --instance router --key-file private-client/evaluator.key run private-client/plan.json --checkpoint private-client/evaluation.json
python3 tools/evaluator/evaluator.py --url http://localhost:8080 --instance router --key-file private-client/evaluator.key status RUN_ID
python3 tools/evaluator/evaluator.py --url http://localhost:8080 --instance router --key-file private-client/evaluator.key cancel RUN_ID
python3 tools/evaluator/evaluator.py --url http://localhost:8080 --instance router --key-file private-client/evaluator.key report --checkpoint private-client/evaluation.json --upload
```

服务端串行 dispatch；输出先 GET、原子存盘验 receipt.output_digest，再 ACK；
默认正文缓存 TTL 300 秒（1 MiB/正文、16 MiB/run），没有正文不能重新生成冒充找回。
显式 retain_outputs 才允许相应持久正文重取，须管理员同意私人留存策略。
finish/cancel 后轮询清理与冻结 receipt set，再做 analysis；执行、cleanup、quality、analysis 四轴分开。
当前评测 purpose 仅 synthetic_only，合成 findings/shortlist 不等于 business_validated，
更不能由 HTTP/登记/ready/发布状态自动推导业务质量。

recipe 写入/不可变版本、export、先 dry_run 再 import、发布/撤回按各自 capability，
详见 [N1 契约](control-api.zh-CN.md#n1) 与 OpenAPI 对应路径。
新建的外来导入总为 foreign/unverified draft，不继承别的机器的质量结论，不覆盖原版本、不自动切换模型；
同身份同摘要的本地既有快照可幂等返回既有记录，不将其降级或重写。
普通 direct key 无需写/发布/评测权限。

recipe 最短操作序列（路径相对 `/api/v1/instances/{name}/`，字段以 OpenAPI 为准）：

| 意图 | 操作与必须核对的结果 |
|---|---|
| 新建 draft | `POST recipes`，提交 description/task_tags、准确 artifact_id/artifact_manifest_digest、engine_identity、完整 deployment/request、conclusion_level/evidence；201 后保存 recipe_id/version/digest，再 GET 明确版本；新建不带旧 recipe_id |
| 修改配置 | 不修改旧版本；POST 同 recipe_id + expected_version 和完整新文档形成下一不可变版本；冲突先 GET，不覆盖历史 |
| 发布/撤回 | `POST recipes/{id}/versions/{v}/publish` 或 `withdraw`，body 为 expected_version/conclusion_level；GET 核 publication_state；发布需要本地身份/固定引擎/对应不可变 findings，不能自称 business_validated |
| 导出 | `GET recipes/{id}/versions/{v}/export` 保存 schema_version=1 快照及摘要；不含权重、凭据或完整运行状态 |
| 导入 | `POST recipes/import` 提交 snapshot 和显式 dry_run=true；核 resolved/冲突，再经许可用同快照 dry_run=false 提交 draft；仅显式 as_new fork 冲突，不覆盖；新外来 draft 不自动可用/发布/加载 |
| 使用/结果 | 核 published/effective_available，选确切 ID/version，按上文 use→infer→release；发布只改目录状态，不切换服务，也不替代有效正文校验 |

这是产品操作，不提供业务评分方法；synthetic_only、执行、清理、外部分析和质量轴不能合并。
旧[单配置测试](deployment-tests.md)通过现有 WebUI/`tests` API 运行并导出 JSON/CSV，不是 N1 持久评测的别名；
两者都会真实加载/生成，仅在具体许可内执行。

<a id="client-faults"></a>

## 5. 故障与不确定结果

| 现象 | 怎么核 | 下一步/责任人 |
|---|---|---|
| 连接拒绝、DNS/TLS/超时 | origin、私网可达性、manager 是否常驻；与 HTTP 鉴权错误分开 | 客户端保留原意图；管理员核服务/网络；不启动第二个 manager |
| 401 | 凭证是否注入、有效、发往正确 origin | 管理员核/续发已批准权限；客户端不盲换 key 重放 |
| 403 | error 类型、实例 scope、controller 与 N1 capability 区别 | 管理员按需核授权；不是未部署的充分证据，不用管理 key 绕过 |
| 404 | 路径、实例、功能配置、对象所有者、boot/保留期 | 管理员核版本/配置；对象不可见不表示从未执行 |
| 409 busy/closing | 当前 operation/use、自己的在途 SSE/评测是否真正结束 | 客户端协调/有界等待读状态，不循环抢占、换 key 或自动驱逐 |
| ai_paused/admission blocked | 当前准入；owner 原因由 owner 确认 | 客户端停止；owner 决定恢复，不自动改 resource-policy |
| recovery_required/unknown/interrupted | boot、stage、持久事实与清理状态 | 管理员核归属、文件/DB后显式恢复；客户端不伪造 verified_stopped |
| epoch_changed/revision_conflict/ready_token_invalid/model_mismatch | 原请求、当前快照/use 身份是否变化 | 客户端停止旧推理/清理；先对账，不能换 boot/token 自动补发 |
| 429/预算耗尽 | 容量、有限预算、已有原 key 事实 | 不换 key 逃避限制；等待或由操作者明确新意图 |
| 200 但空正文、仅 reasoning、length、SSE 未 DONE | 正文、结束原因、完整帧、错误与准确响应身份 | 记失败/截断/不确定；核模板 thinking/输出预算，不盲重发 |
| cleanup failed/unknown/delegated_owner | frozen/terminal、release_operation_id 与 owner receipt | 管理员/owner 核归属释放；不是客户端有权强杀外部程序 |
| 资源 null/stale/available=false | 各指标 source/scope/reason/时间戳 | 记录未知，不写 0；管理员查采集支持，不据此保证容量 |

提交返回前断线时，先保留原 ID/key、body、accepted boot 和本地 checkpoint：

- p3a operation 是当前 boot 内存记录，终态至少保留 24 小时；已知 ID 先 GET。
  丢 202 只能以原 boot/revision/key、完全相同语义的 POST 取原结果。
  boot 变了或历史过期，404 是 unknown，不能改 boot/new key 重跑。
- N1 run/receipt 持久化：已知 run_id 先 GET；丢 202 用原 key + accepted_boot 查询
  evaluation-runs/service-uses。同一 checkpoint 恢复，不自动换 key；跨重启可查历史不代表续执行。
  数据库查询失败不是“不存在”。N1 与 p3a 保留/恢复语义不可混用。
- 普通推理无跨重启 exactly-once 或按请求 ID 找回全文的承诺。传输失败后结果未知，
  不把上述控制幂等机制套在生成上；也不能自动重发会产生业务副作用的应用动作。

资源与测试进一步阅读：[整机/归属进程观测](resources.md)、[owner 暂停与释放](resource-policy.md)、
[旧单配置固定合成测试](deployment-tests.md)。`/api/v1/resources` 是管理权限；
N1 `observation` 需 observe，只返回整机缓存且不含他人进程详情。观测不触发加载、不证明释放或业务质量。

## 6. 复制给客户端开发 AI 的最小交接

把本页及 control-api/OpenAPI 一并交给开发者；另通过私密渠道给 origin、实例名、已部署能力层、
允许的模型逻辑 ID 或 recipe_id/version、凭据安全读取方法、并发协调约定及本次运行授权。
不要附管理 key、机器路径、真实业务材料或私人日志。

实现要求：先只读发现和准入检查；分开 p3a/N1 权限与请求体；202/Running/ready/有效正文分别校验；
绑定准确模型/代际和所有身份 header；有界超时/取消、保留原 key 对账、普通推理不盲重发；
用后按归属释放且 manager 保留；owner/recovery 由有权人员处理；null/false/0/missing 不混淆。
目录、发布、合成结论都不是业务验证。管理员未确认部署/权限时，不擅自改服务器来让示例跑通。
