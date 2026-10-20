# 有限 Router 控制 API（p3a/1）

本接口在原生实例资源上增加有限控制，不是通用任务、调度或远程命令平台。
机读契约：[control-openapi.yaml](control-openapi.yaml)；Python 标准库示例：
[`examples/control_client.py`](../examples/control_client.py)。
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

凭证通过 `Authorization: Bearer <key>` 或 `X-API-Key` 提交：

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

读状态 → 必要时由管理员恢复 → apply → 轮询 operation 成功 → 再读 control
确认同 boot/revision、`state=ready`、准确模型和 token → 推理 → stop。
POST 202 仅表示接受，绝不是 ready 或真实推理成功。响应 `Location` 指向 operation。
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
