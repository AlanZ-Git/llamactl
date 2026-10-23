# 有限 Router 控制与持久评测/recipe API（n1/1）

首次接入先读[客户端接入与使用](client-integration.zh-CN.md)。本文是通用契约，不是具体环境的部署公告；
N1 源码/技术验证不等于目标环境已部署、已授 capability 或业务质量通过。

第 1–7 节描述原有有限控制；第 8 节为可选 N1 扩展。N1 的持久 run、recipe
和 service-use 规则不应与旧 boot 内存 operation 的保留/重试规则混用。

本接口在原生实例资源上增加有限控制，不是通用任务、调度或远程命令平台。
机读契约：[control-openapi.yaml](control-openapi.yaml)；Python 标准库示例：
仓库根 `examples/control_client.py`（仅 p3a/1，非 N1 客户端；N1 见第 8 节）。
单配置有界测试、现有 WebUI 入口和 JSON/CSV 报告见
[deployment-tests.md](deployment-tests.md)；沿用本页控制身份、配置与 owner 准入，不另设配置权威。

## 1. 启用与权限

首版仅支持**一个本机 `llama_cpp` Router**。普通实例以及仅设置
`controlled=true` 的旧准入实例保持原有协议；`Options.control != null`
才启用本协议。模型逻辑 ID、文件和 projector 映射由受信管理员初始化，存入
现有 `options_json`；`preset.ini` 是同一配置的派生文件，不是另一配置源。
公开控制响应只给逻辑 ID/有限参数，不给模型文件、可执行程序、环境或密钥。

管理员初始化使用原生创建接口，不能通过原生 PUT 将现有实例转换为协议实例。
协议启用后，原生 PUT/Delete/Start/Stop/Restart、Router 原生 load/unload 及对应
代理写路径返回 `maintenance_required`（或当前准入错误），管理员也不能绕过。
修改本地映射须在管理员已核对并停止的维护窗口处理，不能由 controller 修改。
旧 WebUI 没有 boot/revision 协议，因此不能直接修改该实例。

必要的受信配置形状（全为合成占位符，不是可直接部署的模型配置）：

```json
{
  "controlled": true,
  "environment_policy": "isolated",
  "backend_type": "llama_cpp",
  "backend_options": {"no_models_autoload": true, "models_max": 1},
  "control": {
    "models": {
      "alpha": {"path": "<administrator-approved-model-file>", "parameters": {}}
    }
  }
}
```

模型数为 1–64，逻辑 ID 为 1–128 个 ASCII 字母/数字/`_-.`，配置的模型 ID
不能以 `.` 开头。`auto_restart=false`、`on_demand_start=false`、`idle_timeout=0`
由受控校验固定；不支持自动预载、自动驱逐、任意后端参数、raw INI/extra_args、
远程模型路径或请求级 autoload。Router 的 `fit=off` 写入派生配置，避免自动
适配改写所选参数。模型文件存在性在执行 apply/load 的预检中检查。

所有新端点始终鉴权；**只要有协议实例，原生管理和私有推理组也强制鉴权**，
即使旧 `require_management_auth` / `require_inference_auth` 开关关闭。
不能用省略凭证绕过 scope。没有协议实例时仍遵循原有开关。
若本地没有可用管理员凭证，先由受信管理员完成本地配置，接口不因此放宽权限。

凭证通过 `Authorization: Bearer <KEY_FROM_SECURE_STORAGE>` 或 `X-API-Key` 提交：

- 管理 key / 已验证的管理员 OIDC session：状态、操作、跨 scope 查询和恢复。
- scoped-controller：固定一个实例的状态、有限 apply/lifecycle、自己的 operation；
  可对该实例作 use-running-only 推理。不能管理密钥、读取全权实例配置、调用任意
  管理/代理写接口或恢复 owner/boot。
- 普通 inference key：不获得控制权限；推理仍需现有实例权限及 ready token。

scoped-controller 沿用管理员 `POST /api/v1/auth/keys/` 创建，示意请求：

```json
{
  "name": "synthetic-controller",
  "controller": true,
  "permission_mode": "per_instance",
  "permissions": [{"instance_id": 123, "can_start": false, "can_evict": false}]
}
```

`instance_id` 必须是已存在的协议实例。仅允许一条 permission，`can_start` 和
`can_evict` 在 controller 下缺省或 null 都解析为 false，显式 true 拒绝。
这两个标志约束自动启动/驱逐，显式生命周期只能走新 operations。
密钥管理仍为原生管理员接口；不要向办公室客户端分发全权 management key。
示例不创建/打印/修改任何长期凭证。

## 2. 端点与基本序列

| 方法、路径 | 作用 |
|---|---|
| `GET /api/v1/instances/{name}/control` | 能力、配置、boot/revision、运行/就绪代际快照 |
| `POST /api/v1/instances/{name}/operations` | 接受或取回 apply/start/stop/load/unload |
| `GET /api/v1/instances/{name}/operations/{id}` | 当前 scope 的操作；管理员可查所有 scope |
| `PUT /api/v1/instances/{name}/control` | 管理员显式核对后的 stopped 恢复，非通用配置 PUT |

读状态 → 必要时由管理员恢复 → 仅在需要时 apply 或 start/load → 轮询 operation 成功 → 再读 control
确认同 boot/revision、`state=ready`、准确模型和 token → 推理 → stop。
POST 202 仅表示接受，绝不是 ready 或真实推理成功。响应 `Location` 指向 operation。
准确模型/配置已 ready 时不必每请求 apply；apply 即使空 patch 也会保存并递增 revision。
新端点响应带 `Cache-Control: no-store`（已通过身份验证后）；勿将 ready token 写日志。

示例操作请求（boot/revision 必须来自当前 GET）：

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

操作请求最大 64 KiB；只接受单个未编码 JSON 对象。重复键、未知字段、尾随 JSON、
错误类型拒绝。顶层字段不接受 null。`expected_revision` 范围 0–9007199254740991；
幂等键 1–128 个 ASCII 字母/数字/`_-.`。JSON 数字的相同精确整数形式（如 `2048`
和 `2.048e3`）等价；字符串数字不等价且拒绝。预算字段为正整数，不能超过状态
公布的 `operation_budget_seconds`，缺省用服务器预算（默认 600 秒）。管理员可设置
`instances.control_operation_timeout_seconds` / `control_operation_capacity`；正值
覆盖默认，默认容量 1024。调用方只能缩短，不允许 0/null 表示无限。

| action | 附加字段 | 行为与 revision |
|---|---|---|
| apply | 必须 model、patch（可 `{}`） | 预检/准备 → stop → 派生文件提交 → DB 保存 → 发布新 revision → start → load → ready；每次成功保存递增一次，即使 patch 为空 |
| start | 禁止 model、patch | 只启动 Router 并等 Router 健康，不自动 load；已运行时 `already_running`，不清除现有 ready |
| load | 必须 model；禁止 patch | 仅对已运行 Router 加载已批准配置；另一模型已加载时拒绝，需先 unload；同模型重新确认也生成新 token |
| unload | model 可省略，若提供必须匹配清理目标；禁止 patch | 卸载已知模型并确认 unloaded；不改变持久配置，不证明进程树退出，不清除故障恢复屏障 |
| stop | 禁止 model、patch | 温和停止本代归属树，等待退出；无 force，不保证显存瞬时释放 |

只有 apply 改配置 revision。`target_model` 是最近 apply 持久选择的模型；
`actual_model` 是本代运行/清理身份。unload 后 load 另一已批准模型不改 target 或
revision；推理必须使用 **actual_model**，不能用持久 target 猜当前模型。

<a id="finite-parameters"></a>

## 3. 有限参数

参数均为 **per-model**，不会同时传作 Router 全局命令行参数。
`GET control.parameters` 返回与下表相同的类型、范围、nullable 和枚举。
`configured_parameters` 给各模型的显式覆盖值，不假装是所有引擎有效默认值。

| patch 字段 | 类型、范围 | null | 引擎项 |
|---|---|---|---|
| context | integer，0–1048576 | 清除覆盖 | ctx-size |
| gpu_layers | integer，0–999 | 清除覆盖 | gpu-layers |
| threads | integer，0–1024 | 清除覆盖 | threads |
| batch | integer，1–65536 | 清除覆盖 | batch-size |
| ubatch | integer，1–65536 | 清除覆盖 | ubatch-size |
| cache_ram | integer，0–1048576 MiB | 清除覆盖 | cache-ram |
| kv_type_k | string：f16 / q8_0 / q4_0 | 清除覆盖 | cache-type-k |
| kv_type_v | string：f16 / q8_0 / q4_0 | 清除覆盖 | cache-type-v |
| flash_attention | boolean | 不允许 | flash-attn=on/off |

missing 保持原覆盖；0/false 明确保留；null 仅对声明可清除字段移除覆盖，之后用
固定引擎默认，不是写入字符串 `null`。0 的引擎意义取决于字段，例如 threads=0
由引擎自动选择，gpu_layers=0 为不卸载 GPU 层；不是所有字段都允许 0。

组合校验：ubatch ≤ batch，清除/缺省参与校验时采用固定引擎默认 512/2048；
量化 V cache 必须在最终配置中显式 `flash_attention=true`。范围是协议上界，
不是当前硬件承受能力或性能保证。无效组合、模型 ID/文件不存在、未知参数等均在
停旧进程之前拒绝；候选文件准备失败也保持旧 revision/ready/进程。

## 4. ready、推理与 owner 暂停

ready 发布需要：本代归属进程存在、派生 preset 与配置一致、Router `/health`
成功、`/models` 精确目标 loaded（无其他 loaded 模型）、目标
`/props?model=<ID>&autoload=false` 可用并有 `default_generation_settings`。
依据固定官方 llama.cpp b11717 路由能力：`/health?model=...` **不是**子模型探针。
目标等待受总 deadline/owner 取消约束，不发送生成请求；子模型暂时 503 可继续等。

`GET control` 是**最近确认状态 + 当前管理器身份/owner 代际校验**，不访问引擎，
不测实时子模型健康。外部子模型独立崩溃而 Router 父进程仍活着时，快照可能仍是
最近确认的 ready；推理会透传实际后端失败，不能把 token 当成持续可用承诺。
父进程退出、配置身份变化、生命周期开始、owner 代际变化使旧 token 失效。
真实推理成功和 Windows/GPU 能力必须另行验证，不能由 ready 推定。

聚合 OpenAI 路由使用 `model="<instance>/<logical-model>"`：

```http
POST /v1/chat/completions
Authorization: Bearer <scoped-key>
X-Ready-Token: <current-token>
Content-Type: application/json

{"model":"router/alpha","messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":8,"stream":false}
```

逐实例 `/llama-cpp/{name}/v1/...`、原生 `/completion` 等和管理员实例 proxy
使用未加实例前缀的准确逻辑 ID，例如 `alpha`。所有允许的推理路径都在共享使用
许可内检查 model/token；最终转发统一剥离 `X-Ready-Token`，不传给引擎。
只支持既有准入白名单的 completion/chat/embedding/rerank/infill/tokenize/
detokenize/apply-template 类路径；未知可变路径、查询参数、重复 JSON 键和
请求级 autoload 覆盖拒绝。推理体沿既有受控限制最大 16 MiB。

普通维护与推理使用同一 gate，已有 SSE 未退出转发路径前维护立即 `busy`，不排队。
owner 暂停立即挡新工作并取消后台加载（包括温和暂停）；现有 SSE 按原 owner
温和/立即释放契约收束。恢复准入不自动启动、不重放、不复活旧 token。scoped
controller 不能取消暂停。详见 [resource-policy.md](resource-policy.md)。

## 5. 幂等、持久化与恢复

幂等身份为 `(已鉴权 scope, instance, boot_id, idempotency_key)`。服务端先校验并
规范化 payload，然后检查当前 boot、查同 key，最后才对新请求检查 revision/busy。
同 payload 返回同 ID/原有结果（即使 expected_revision 已旧）；异 payload 409。
missing/null/0/false 区分；省略 timeout 与显式写入默认 timeout 也不同。
POST 响应丢失只能以**完全相同语义的原请求**重发，不换 key/revision/boot。

内存表按结束时间保留至少 24 小时；运行记录不驱逐。窗口内达到容量则 429，
不会删除旧记录腾位。过期查询 404 不等于从未执行。管理员重启生成新 boot，
旧操作终态不恢复、旧 token 全部丢弃；没有跨重启 exactly-once/自动续接保证。

文件、DB、进程**不是事务**：apply 先准备候选再 stop，文件替换后才保存 DB，
保存成功才发布 revision。保存失败可留下候选 preset 与旧 DB 不一致，状态 failed、
不启动、不接受新 revision。若取消发生于已成功保存期间，公布真实已保存 revision，
但不继续启动。旧配置保留在本轮内存、操作 stage 给出失败位置；无自动回滚循环。

- `failed`：查 stage/错误，保留故障。明确 stop 确认归属树退出，或显式 apply 已知
  配置（仍先 stop，失败不得继续），不是重放旧请求。unload 不清除故障屏障。
- `unknown`（含每次 boot）：不能发新工作，不能简单换 boot 重发旧指令。
- `interrupted` / operation `deadline_exceeded`：可清理已知归属树，但即使 stop
  成功仍需管理员恢复，不能由 controller 自动继续。

管理员必须先核对当前进程归属、文件与 DB；未知残留进程不能被接口自动接管或杀掉。
确认停止后提交：

```json
{"boot_id":"<current-boot>","expected_revision":1,"verified_stopped":true}
```

至 `PUT .../control`。服务端再次核对本代非 running、归属树/monitor 已退出、端口
可用及存在的 preset 与保存值相等；不写 preset、不启动、不生成 ready，也不恢复
owner。缺少派生文件可在后续显式启动/应用时重建；存在但不一致则 `preset_mismatch`，
管理员先在停止状态依据持久权威修复。恢复请求不是幂等 operation，响应丢失先 GET
核对，不把 `verified_stopped=true` 当作 OS 检查的替代。

## 6. 状态与错误

control 状态：`unknown/stopped/running/ready/failed/interrupted`，另有 `running`
布尔、`recovery_required`、`admission=allowed/blocked`、`active_operation`。
运行中的 operation 状态是 `running`，终态为 `succeeded/failed/deadline_exceeded/interrupted`；
保留最后 stage（accepted/prepare/stop/persist/start/router_health/load/ready_probe/unload），
以及 started_at/deadline/ended_at。可选值缺省不发送，不用 null/0 伪造 token/模型。
终态 operation 的 token 是历史结果，仍须读当前 control；不保证它还有效。

新端点错误形状为 `{"error":"<code>","details":"<safe-detail>"}`；`ai_paused`
另含 `retryable=false`。原生鉴权/推理路由保留其原错误格式，不能硬套此形状。

| HTTP | 常见业务码 / 意义 |
|---|---|
| 401 | authentication_required；原生管理鉴权也可能返回其 authentication_error |
| 403 | controller_required / permission_denied / administrator_required |
| 404 | instance_not_found / control_not_enabled / operation_not_found（不泄漏其他 scope） |
| 409 | epoch_changed / revision_conflict / idempotency_conflict / busy / ai_paused / recovery_required / already_running / model_already_loaded / model_mismatch / revision_exhausted / manager_stopping |
| 409（恢复） | recovery_requires_stopped / owned_tree_not_exited / port_ownership_unknown / preset_mismatch / preset_sync_failed |
| 422 | invalid_config / unsupported_parameter / invalid_deadline |
| 429 | operation_capacity；不删除窗口内结果，请勿换 key 循环重试 |
| 503 | control_unavailable；推理暂停为 503 ai_paused，停止实例推理为 instance_not_running |
| 500 | control_failed；未分类内部错误 |

后台失败不改历史 POST 202，而在 operation `error_code` 中体现：如
preset_sync_failed、persist_failed、stop_failed、start_failed、model_load_failed、
model_unload_failed、deadline_exceeded、interrupted。查看 stage 并执行明确恢复，
不能把 failed/超时视为“什么也没做”。推理 token 缺失/过期为 409
ready_token_invalid，模型不匹配为 409 model_mismatch（聚合入口缺 model 为原生 400）。

## 7. 示例与验证边界

```sh
# 管理员已初始化并完成显式恢复；凭证从安全环境注入，勿写入命令历史。
python3 examples/control_client.py --base-url http://127.0.0.1:8080 \
  --instance router --model alpha --patch '{"context":2048,"gpu_layers":0,"flash_attention":false}'
```

示例读取 `LLAMACTL_CONTROL_KEY`，执行有限 apply → ready → 一次合成请求 → stop；
默认不流式，可加 `--stream`。不自动恢复 boot/owner、不创建凭证、不盲重放。
传输失败仅有限重发同一 POST 字节；业务错误、状态变化或 operation 超时直接退出非零，
不会悄悄换 boot/revision。失败后模型可能仍在运行，须按状态执行显式清理。
不要把示例当生产重试/凭证存储 SDK。

本协议有 Linux 合成进程、真实本地 HTTP/SSE、并发与 schema 测试；这些不是
Windows 执行、GPU 实测、办公室端到端或日常部署验收。

<a id="n1"></a>

## 8. N1：发现 → 有界评测 → 持久 recipe → 直接服务使用

### 配置与权限

管理员在本地 `n1` 配置中指定 `instance`、`roots: {root_id: approved_absolute_root}`
和固定 `engine_identity`；远程 API 不能提供执行程序、根路径、URL 或 shell。
仍复用现有 SQLite，迁移 004 增加 artifact、recipe、run/event、capability 数据。
Options 是当前部署权威；recipe 是不可变版本目录。不要在线复制活动 SQLite 主文件当备份；
正式迁移/回退需停写、一致备份并成套保全二进制、配置、DB/WAL 与 preset，另行授权。

管理员 `PUT .../capabilities/{key_id}` 写入 `{"capabilities":[...]}`，能力为：
`discover/evaluate/recipe_write/recipe_publish/service_use/inference/observe`。
旧 controller 不自动获得这些能力。`evaluate` 包含自己的查询、finish/cancel/heartbeat/analysis；
`service_use` 包含自己的 release/有限 renew。不另发取消权限。推理还需原实例推理权限，
并且凭证 scope 必须与 use 所有者相同。能力撤销会主动停止派发并清理活跃对象。

直连客户端最小 key 形状：`controller=false`、`permission_mode=per_instance`，仅目标实例
推理权限（`can_start=false/can_evict=false`），加该实例 N1 `discover/service_use/inference`。
`GET inventory` 返回 `roots/boot_id/revision`，供客户端生成运行 envelope，不需要旧 `GET control`
或 operations 权限。评测客户端另需 `evaluate`，写/发布另按需授予；`observe` 仅用于资源预检。
自己的安全 cancel/release 不校验旧 revision，也不被 paused 或预算耗尽挡住；他人对象不可操作。

以下均省略 `/api/v1/instances/{name}/` 前缀；具体字段见 OpenAPI：

| 路径 | 方法、用途 | 能力 |
|---|---|---|
| inventory | GET 批准 root ID（不是完整候选列表）与 boot_id/revision | discover |
| inventory/scans | POST root_id，可选 filename 单文件；或 scan_id+continuation 接续 partial | discover |
| inventory/scans/{scan_id} | GET offset/limit 冻结分页 | discover |
| inventory/verify | POST boot/revision/key、scan_id/candidate_id，可显式 projector_id | evaluate |
| inventory/register | POST boot/revision/key、artifact_id，maintenance/CAS 登记 | evaluate |
| recipes | GET 描述目录；POST 不可变 draft 或下一版本 | discover / recipe_write |
| recipes/{id}/versions/{v} | GET 明确版本 | discover |
| recipes/{id}/versions/{v}/publish、withdraw | POST expected_version/conclusion_level，不切换模型 | recipe_publish |
| recipes/{id}/versions/{v}/export、recipes/import | GET 快照；POST dry_run=true 预览后 false 提交 draft | discover / recipe_write |
| evaluation-runs | POST 有限 manifest；GET 历史或 key+accepted_boot 找丢失 202 | evaluate |
| evaluation-runs/{id} | GET 事实、事件、冻结摘要 | evaluate，本人或管理员 |
| evaluation-runs/{id}/attempts | POST 明确 seq、候选与合成 messages，服务端内部串行执行 | evaluate |
| evaluation-runs/{id}/attempts/{seq}、.../ack | GET 非消费式取正文；POST digest ACK | evaluate |
| evaluation-runs/{id}/finish、cancel、heartbeat、analysis | POST 清理、取消、有限续期或外部分析 | evaluate |
| service-uses、service-uses/{id} | POST 明确 recipe_id/version，GET switch/ready 身份 | service_use |
| service-uses/{id}/release、renew | POST 本人收束或有限续租 | service_use |
| observation | GET 共享 sampler 的整机缓存，无其他进程详情 | observe |

N1 handler 请求体最大 1 MiB，拒绝编码体、重复键、未知字段和不合法 null。
N1 的 inventory/recipe 发现是只读、不加载；capability 不足为 403 `capability_required`，
不是“服务器一定未部署”。N1 direct key 不必有旧 control/operations 权限。
release/cancel 的安全 envelope 仍须当前 boot 且匹配原 run boot，跨 boot 只查持久历史，不能自动续跑。

### 目录、版本与真实性

扫描不加载。metadata 损坏、未知架构、复制中、缺片、辅助 projector、链接都应可见；
不把第一页或 partial 当全部。所有 symlink/reparse（包括根内）拒绝。分片按 metadata
和严格命名交叉核验，projector 显式关联。首次/改变后 verify 全 SHA256，之后使用文件 ID、
大小、mtime、头摘要与成员集合轻量校验，不每次重 hash 大权重。缺失/变化不自动改绑。

已知文件可用 `POST inventory/scans` 提交
`{"root_id":"fixture","filename":"synthetic.gguf"}`，只通过现有安全打开、metadata 解析与冻结
快照路径处理这个 basename；不枚举、不解析同根其他候选。省略或空 `filename` 保持原有全根递归
扫描。只允许单个 basename，不接受绝对路径、`/` 或 `\\`、遍历、盘符/ADS 的 `:`、NUL、前导点、
尾随点/空格或目录；无效选择为 422 `invalid_config`。不存在仍产生唯一 `unreadable/stat_failed`
候选，链接为 `blocked_link`，metadata 损坏为 `invalid/gguf_structure_invalid`，不悄悄省略。
具名 split 的兄弟片未在快照内时为 `incomplete`，不回退全根扫描，即使磁盘上有兄弟片。
响应 `filename` 回显单文件选择，分页与续扫保持不变。续扫省略/空 filename 保留原选择，
重复同名允许，冲突 filename 为 422 且不消耗 continuation；不能用续扫扩大或换选目标。

`identity_level=verified`、`file_state=complete` 和 register 成功**只证明内容/成员身份与有限
metadata 一致性**，不证明权重有效、引擎支持、可加载、ready 或质量。解析保留 metadata/descriptor
长度、数量、维度上限及 split 一致性检查；不复制引擎的量化类型表，不验证 tensor payload 的几何、
对齐或权重完整性。未知 tensor type、权重尾部截断但 metadata/descriptor 可读的文件仍可能完成
verify/register；完整文件 SHA256 绑定的是现有字节，不是供应商完整性认证。register 不启动 Router、
不加载、不生成。后续真实加载失败由 evaluation attempt 的失败回执或 service-use 的失败终态记录，
不会转成 ready/质量成功；cleanup 是否释放须独立查看。发布仍需不可变评测证据，登记本身不授予发布。
`qwen35/qwen35moe` 是可识别 metadata，不等于已通过目标引擎加载或质量验证。
完整 split 仅首片是逻辑模型；缺片、混架构/名称/索引、损坏成员、重复 tensor 名及 verify 前 metadata
漂移拒绝。projector 类型错误或已支持的 embedding/projection 维度冲突拒绝；其他显式关联只能确认
内容身份，`projector_compatibility=unverified`。当前没有固定引擎多模态兼容探针，故关联候选不能
register/执行/发布，其他纯文本候选不受影响。不能把通用 `clip` 类型或维度相等当成兼容证明。

Windows 已有安全打开/最终句柄路径/卷身份检查实现；**Linux GREEN 不证明 Windows
reparse 行为或引擎执行期间持读句柄兼容性**。当前执行采用管理员只读投放约定，Windows
需显式配置 `administrator_read_only_placement: true`。这不是抗恶意本机写入保证。

recipe 全量替换目标白名单参数：missing 不继承上一用途；0/false 保留，声明 nullable 的
null 清除覆盖。应用时 provenance 与 revision 同事务保存；旧有限参数修改清除 provenance。
发布需要本地 verified/可解析 artifact、固定引擎身份及具体不可变 findings 引用。
外来导入始终 foreign/unverified draft；未解析不能运行，不能继承其他机器的质量结论。
显式 as_new 可 fork 冲突，不覆盖原版本。目录返回 `effective_available`，不是“业务已验”。

### 有界执行与输出交付

evaluation 明确候选、用途/fixture 版本、整数参数范围、wall/attempt/cleanup 秒数、总 requests/
tokens/bytes，以及真实通知 `notice_ref/not_before`。当前执行用途仅 `synthetic_only`。
先持久接受、预留预算和 dispatch intent，再 apply/start/load/generate；一个协调器串行，
busy 直接 409，无排队或抢占。usage 未知保留最大预留，失败不返还为无限重试额度。
分析与执行成功独立，空正文、仅 reasoning、截断、失败均不能成为有效质量输出。
生成回执还核对 ready/目标 props 所绑定的请求 model 与响应 model；缺失或不匹配拒绝。
当前文本合同只接受 `finish_reason=stop` 为完成，`length` 为 truncated，缺失/其他结束原因失败。
接受过的同 key 请求先查持久事实再检查当前适用性，后续 registration/revision 或撤回不导致重执行；
数据库查询失败不等于不存在。service-use 在 ready 事件持久化成功后才开放实际推理准入。

默认正文在有限内存 buffer：TTL 300 秒、单正文 ≤1 MiB、run 总 ≤16 MiB；管理员可收窄。
鉴权重复 GET 不消费，客户端应先原子保存并验 SHA256，再用 receipt.output_digest ACK。
跨重启/过期重取需显式 `retain_outputs=true`（仍受预算）；否则 `output_unavailable`，
不能伪造正文或自动再生成。保留全文是显式私人存储策略，不写日志或公开仓库；本阶段不自动删除
持久留存正文，实际留存维护窗口须另行规划。普通推理不承诺跨重启 exactly-once。

finish/cancel 先触发归属清理；冻结 receipt set 后才有默认 24 小时 analysis 窗口，不等分析占 GPU。
`findings_v1` 对 manifest 所有候选逐项声明 tested/untested、receipt 引用、参数/身份摘要、
限制与 stop_reason。服务端复算引用/摘要，语义判断标 external。相同版本/payload 幂等，
不同 payload 409；更正用新 analysis_version+supersedes。窗口内允许跨 boot 补报告但绝不加载。
缺失分析是 awaiting/not_available，不自动推荐。执行、cleanup、quality、analysis 四轴分别读取。

服务使用 idle 默认 60 秒、最多 300 秒，绝对期限最多 30 分钟；renew 不延长绝对期限。
renew 可带 `idle_seconds`（1–300，省略或 0 选 60），从本次续租更新 idle 时长；不复活已过期 use。
若 use 曾选择非默认 idle，续租时也应显式传所需时长，不能假设省略就保持原时长。
句间 active_requests=0 不释放 reservation。closing/到期先拒新请求，已有转发真正退出、
maintenance 退出且清理确认后才开放下一 use。普通推理硬 deadline 最多 600 秒并取 use
绝对截止的更早值。owner pause 立即撤销工作；ReleaseTicket 接管归属树时旧 run 只记唯一
release_operation_id 引用和 owner receipt 投影，旧 epoch 清理不能越过 resume 操作新代际。
unknown/失败清理保留恢复屏障；borrowed 既有 ready 不卸载他人加载。

### 推理的冻结 prompt 与完整响应身份

N1 首版仅接受 chat completions（聚合、native、instance proxy 同样约束），非任意 backend API。
messages 为 1–256 项文本消息，每项仅 `role/content`，role 为 system/user/assistant，content 为字符串；
不接受图像数组、tool 消息或任意额外生成字段。SSE 可由 `stream=true` 请求；工具 Client.infer 只处理非流式 JSON。
请求 contract 示例：

```json
{"parameters":{"max_tokens":8,"temperature":0,"chat_template_kwargs":{"enable_thinking":false}},
 "system_prefix":"synthetic only","template_version":"synthetic-v1"}
```

嵌套白名单只有 `enable_thinking`；false 是明确值，missing 由 recipe 填充。
未知键、重复键、null、与冻结值冲突一律拒绝。非空 system_prefix 在首条没有 system 时插入；
已有首条 system 必须完全相同。template_version 是客户端 prompt 协议版本，不允许覆写引擎模板；
实际模板仍由固定引擎/模型内容身份绑定。

ready 的 service-use GET 返回 `request_contract`、`prompt_digest`、`identity`。
`prompt_digest = SHA256(encoding/json 规范化的 {system_prefix,template_version})`，键排序、
紧凑 UTF-8，遵循 Go 的 HTML/U+2028/U+2029 转义。推荐直接使用 GET 返回值，并由客户端复算校验。

```http
POST /v1/chat/completions
Authorization: Bearer <same-use-owner-inference-key>
X-Service-Use: <use-id>
X-Ready-Token: <ready-token>
X-Prompt-Template-Version: synthetic-v1
X-Prompt-Digest: <service-use-prompt-digest>
X-Request-Timeout-Seconds: 30
Content-Type: application/json

{"model":"router/<artifact-id>","messages":[{"role":"user","content":"synthetic test"}]}
```

native `/llama-cpp/{name}/v1/chat/completions` 和 instance proxy 的 model 不带 `router/`。
每个 binding header 必须且仅能一个；旧 token/版本/digest 错误不会转发。内部 header 全部
在 backend 前剥离。响应带 `X-Boot-ID/X-Config-Revision/X-Recipe-ID/X-Recipe-Version/
X-Process-Generation/X-Load-Generation/X-Prompt-Template-Version/X-Prompt-Digest`，
来自准入时身份而不是稍后的快照，也不允许 backend 冒充这些值。

### 标准库薄客户端与可运行合成 HTTP 夹具

工具说明：[N1 客户端与外部评测](evaluator-client.md)（同源于仓库 `tools/evaluator/README.md`）。无需 evaluator daemon：
`discover` → 选择描述和明确 ID/version → `use`/轮询 → `infer` → `release`。
`plan/run/status/cancel/report` 支持有限串行候选与私人 checkpoint。检查点不是事实账本；
丢失 202 先 key+accepted_boot 查询，同一 checkpoint 不换 key 重开。ACK 后缓存正文不被
服务器的 output_unavailable 覆盖；缓存摘要不符立即停止。没有正文或外部合成判定的候选
不列为已验证推荐。普通 infer 传输失败不重试。

```sh
# 只启动隔离 temp DB、随机 loopback 端口和测试二进制模拟 Router，不访问日常实例。
go test ./pkg/server -run '^TestN1PythonHTTPFixture$' -count=1 -v -timeout=120s
python3 -m unittest discover -s tools/evaluator/tests
```

Go 夹具调用 `synthetic_http.py`，真实经过 discover/run/report/use/infer/release HTTP 路由，
含重连查询、不重放、ACK 后正文保全及响应身份核验。只证明 synthetic_only 本地链路。

### 真实负载前的短窗 preflight（本阶段不运行真实负载）

`preflight.py` 只读取既有 observation，不另建 collector。真实 run 包必须提供多次 distinct
fresh 的整机 CPU/RAM/GPU/VRAM 百分比、RAM/VRAM 已用/可用量、各指标时间戳、采样间隔与 gap；
阈值及模型/参数需求估算明确标为工程建议，不是用户限制。至少两次新鲜样本才走正常低负载路径，
重复读缓存不是多次采样，也不足以证明容量/稳态。

高占用、余量不足或关键指标未知时先 prepared，报告实际影响，由用户选择自行清资源或保留 co-load，
明确确认本次 run。warning 一经触发即持久锁存：等待或数值自行下降不能代替确认。用户知情确认后，
同一预算内可继续，即使仍高/不足/未知；再次采样只记录，不重新设资源审批门槛。
低负载仍是既有授权+真实提前通知，不增加普遍审批。owner/身份/隔离/有限预算和实际失控安全失败
不被该确认覆盖。客户端不关闭其他进程，不伪造 notice/确认；实际通知和确认由操作者的既有渠道完成。
