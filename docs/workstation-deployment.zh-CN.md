# 工作站部署指南（Windows / Linux）

目标：在一台新工作站上，使用明确版本的本仓库和官方 `llama-server`，独立提供模型管理与推理服务。
构建机可以与运行机分开；日常运行不依赖构建机在线。本文不执行部署，也不宣布定制发布包已存在。
其余后端、普通实例与 remote-node 配置保留在[安装说明](installation.md)及[配置参考](configuration.md)，
不等于本机 control/N1 已支持跨节点调度或统一 GPU 池。

## 1. 先固定输入和运行边界

1. 从维护者取得准确的 fork 源码地址与完整提交 ID，或带文件清单/摘要的候选源码目录。
   不以本 README 的上游徽章、上游 `latest`、Docker 镜像或某个分支名证明包含定制能力。
   没有定制发布包时，按下文从固定源码构建；不要虚构下载地址。
2. 在取得的仓库内记录 `git rev-parse HEAD`、`git status --short`。预期提交必须与维护者交付一致；
   dirty 候选另核其文件清单，HEAD 不能标识未提交内容。不要清理已有改动或在日常目录强行 checkout。
3. 固定官方 llama.cpp 引擎包/源码版本、平台/架构、CPU 或 GPU 构建类型与文件摘要。
   本控制契约的引擎依据是 **b11717**；更换版本要重新验证 Router 行为，而不是随手跟随 latest。
   核对其 `llama-server --version` / `--help`，仅版本查询不证明能加载模型。
   保留包内依赖 DLL/共享库，不只复制一个 EXE；驱动不兼容先报告，不擅改驱动。
4. 准备管理员批准的模型文件与许可证，核来源摘要、分片完整性、磁盘/内存/显存余量。
   GGUF 文件名不是模型身份。N1 当前可执行路径是已核验文本模型；关联 projector 的候选因兼容性未验证不能登记/执行/发布，
   不把上游支持视觉等同于 N1 支持。不要照抄他人模型路径或推荐 GPU 层数。
5. 新建独立、访问受限的运行目录；配置、数据、缓存和端口不能指向已有日常实例。
   凭据由本机操作者通过安全存储注入，管理 key 与客户端 key 分开，不复制他人的数据库/密钥。

普通本地构建和独立配置可按既有授权连续做。替换现有服务、迁移现有 DB、安装系统依赖/驱动、
改变防火墙/私网/TLS、自启、删除或远程发布，先核相应具体授权；本文不是这些操作的授权。

## 2. 构建管理器

工具链以 `go.mod`、`webui/package-lock.json` 和 `.github/workflows/release.yaml` 为准：
Go **1.26.0 或兼容的 1.26 补丁版**、Node.js **22（满足锁定依赖的 engines，至少 22.12）**与 npm、Git，
以及目标平台可用的 C 编译器。SQLite 使用 `github.com/mattn/go-sqlite3`，必须 **CGO_ENABLED=1**。
不使用 CGO=0 的“可编译但 SQLite 不能运行”产物。

先构建 WebUI，再编 Go；`webui/webui.go` 将 `dist/*` 嵌入二进制。
旧 dist 也会被嵌入，故须确认它由本次源码生成。`npm ci` 使用锁文件，不擅自升级依赖。
构建会获取依赖；在获准联网、具有相应工具链的环境执行，失败保留日志，不降级工具链凑成功。

### Windows 原机构建（PowerShell）

在源码根目录执行；已安装并获准使用适配当前 Go/架构的 MinGW-w64 GCC（本例为 Windows amd64）。
用 `Get-Command` 确认当前会话解析的是预期工具，不修改系统 PATH/全局配置：

```powershell
go version
node --version
npm --version
Get-Command go, node, npm, gcc
gcc --version
git rev-parse HEAD
git status --short
$env:CGO_ENABLED = '1'
$env:CC = (Get-Command gcc).Source
go env GOOS GOARCH CGO_ENABLED CC
Push-Location webui
npm ci
if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' }
npm run build
if ($LASTEXITCODE -ne 0) { throw 'WebUI build failed' }
Pop-Location
New-Item -ItemType Directory -Force ..\llamactl-build | Out-Null
go build -o ..\llamactl-build\llamactl.exe ./cmd/server
if ($LASTEXITCODE -ne 0) { throw 'Go build failed' }
..\llamactl-build\llamactl.exe --version
Get-FileHash ..\llamactl-build\llamactl.exe -Algorithm SHA256
```

确认 `GOOS=windows`、`GOARCH=amd64`（或明确的目标架构）；其他架构不直接套用 amd64 GCC。
从 Linux 交叉构建 Windows 还需目标 MinGW 编译器、`GOOS/GOARCH/CC/CGO_ENABLED` 和目标机运行验收，
不是仅设 `GOOS=windows` 就完成。本文优先原机构建，不要求新机另造交叉工具链。

### Linux 原机构建（POSIX shell）

已有获准的 GCC/C 开发环境；下面每步失败即停止：

```sh
set -eu
go version
node --version
npm --version
cc --version
git rev-parse HEAD
git status --short
export CGO_ENABLED=1
export CC=cc
go env GOOS GOARCH CGO_ENABLED CC
(cd webui && npm ci && npm run build)
mkdir -p ../llamactl-build
go build -o ../llamactl-build/llamactl ./cmd/server
../llamactl-build/llamactl --version
sha256sum ../llamactl-build/llamactl
```

两种普通 `go build` 默认的 `--version` 信息为 unknown；它不是源码身份丢失的错误，也不是固定版本证明。
保留提交/候选清单、构建日志、产物 SHA256；若注入元数据，真实变量名是
`main.version`、`main.commitHash`、`main.buildTime`（见 `cmd/server/main.go`）。不把自填版本串当验收。

<a id="local-config"></a>

## 3. 独立状态与本机鉴权配置

把构建产物及必要运行库放入独立运行目录，在该目录准备 `config.yaml`。
下例只用于初始化 manager，不加载模型。所有绝对路径由本机环境变量给出；不要直接提交含真实值的配置。

```yaml
server:
  host: "127.0.0.1"
  port: 8080
  allowed_origins: ["http://127.0.0.1:8080"]
  allowed_headers: ["Authorization", "Content-Type", "X-API-Key", "X-Ready-Token", "X-Service-Use", "X-Prompt-Template-Version", "X-Prompt-Digest", "X-Request-Timeout-Seconds"]
data_dir: "${WORKSTATION_STATE}"
database:
  path: "${WORKSTATION_STATE}/llamactl.db"
instances:
  instances_dir: "${WORKSTATION_STATE}/instances"
  logs_dir: "${WORKSTATION_STATE}/logs"
  port_range: [8100, 8200]
  default_auto_restart: false
  default_on_demand_start: false
  default_idle_timeout: 0
backends:
  llama-cpp:
    command: "${WORKSTATION_LLAMA_SERVER}"
    cache_dir: "${WORKSTATION_CACHE}"
    args: []
    docker:
      enabled: false
auth:
  require_management_auth: true
  require_inference_auth: true
```

- `WORKSTATION_STATE` / `WORKSTATION_CACHE`：本机独立绝对目录；`WORKSTATION_LLAMA_SERVER`：固定引擎绝对路径。
  Windows 可用正斜杠路径避免 YAML 反斜杠转义。端口只是示例，先确认空闲。
- 通过受保护环境注入新的、明确配置的 `LLAMACTL_MANAGEMENT_KEYS`（一项或逗号分隔多项）。
  不在命令历史或本仓库写 key；不要依赖启动日志打印的 fallback key，它没有 owner 暂停/恢复权限。
  安全存储如何提供值由本机操作者决定，不能拿占位符充当真实密钥。
- 显式设置 `LLAMACTL_CONFIG_PATH` 为本文件的绝对路径。环境 `LLAMACTL_*` 最后覆盖 YAML；
  `.env` 会自动加载，也可能影响结果。用干净的本次会话/运行目录，核已有覆盖，不读取或打印无关秘密。
- 所有变量必须已设置。`${VAR}` 未设置时不会自动报缺失，可能留下字面量；源码也不保证配置读失败就退出。
  启动日志必须显示读到预期配置，不能将“监听了”当配置正确。
  不依赖默认目录、`~` 展开或相对路径归属；这里明确给出所有状态路径。

Windows 会话设置非秘密路径示例（目录均为虚构，可换成本机获准空目录）：

```powershell
$env:WORKSTATION_STATE = 'C:/llamactl-workstation/state'
$env:WORKSTATION_CACHE = 'C:/llamactl-workstation/cache'
$env:WORKSTATION_LLAMA_SERVER = 'C:/llamactl-workstation/engine/llama-server.exe'
$env:LLAMACTL_CONFIG_PATH = (Resolve-Path .\config.yaml).Path
# LLAMACTL_MANAGEMENT_KEYS 已通过安全存储注入，不在此赋值或打印。
.\llamactl.exe
```

Linux 对应示例：

```sh
export WORKSTATION_STATE="$HOME/llamactl-workstation/state"
export WORKSTATION_CACHE="$HOME/llamactl-workstation/cache"
export WORKSTATION_LLAMA_SERVER="$HOME/llamactl-workstation/engine/llama-server"
export LLAMACTL_CONFIG_PATH="$PWD/config.yaml"
# LLAMACTL_MANAGEMENT_KEYS 已通过安全存储注入。
./llamactl
```

首次启动会创建目录、打开 SQLite 并自动运行仓库内迁移（含 004），不是只读检查。
不要先对旧状态试启动。不要假设有 `llamactl install/start/stop` 等服务 CLI；此入口是前台管理器。
系统服务/自启另行配置和授权，不是本指南前置。

<a id="instance-setup"></a>

## 4. 注册实例和模型：选择一种协议

先打开 `http://127.0.0.1:8080`，用明确配置的管理 key 登录。核本机 manager、版本、配置来源，
初始实例列表应为空；意外看到旧实例就停止初始化并查数据目录，不覆盖或删除。
管理 API `GET /api/v1/version`、`GET /api/v1/instances` 均需管理凭据；版本响应不代表模型就绪。
配置接口/日志可能含本机路径，不把其原文贴到公开问题或源码。

普通实例通过 [WebUI/原生接口](managing-instances.md)登记模型路径与 backend 参数；
这是普通自动启动/驱逐流程，不是下面的控制协议。

### control（p3a/1）

受信管理员用 `POST /api/v1/instances/router` 创建新实例，body 是 Options 本身，不包一层 `options`。
合成形状如下，提交前必须将路径替换为本机已批准文件：

```json
{
  "controlled": true,
  "environment_policy": "isolated",
  "auto_restart": false,
  "on_demand_start": false,
  "idle_timeout": 0,
  "backend_type": "llama_cpp",
  "backend_options": {"no_models_autoload": true, "models_max": 1},
  "control": {
    "models": {"alpha": {"path": "<approved-absolute-model-file>", "parameters": {}}}
  }
}
```

不带 `nodes` 即本机。只支持一个本机原生 llama.cpp Router；`controlled` 单独为 true 只是旧准入层，
非 control 协议。协议模式不能用旧 UI/原生 PUT/Start/Stop 或 raw Router load/unload 日常操作。
不填写额外 raw 参数、外部 preset、自动加载/自动重启；有限参数作用域、零值与 null 见[控制契约](control-api.zh-CN.md#finite-parameters)。

每次 manager 启动会换 boot，并保留恢复屏障。管理员先核进程归属、端口、DB/派生 preset 一致且确已停止，
读 `GET .../control`，再按[显式恢复](control-api.zh-CN.md)提交当前 boot/revision 与 `verified_stopped=true`。
这个字段是事实确认，不是让客户端自动绕过未知进程。遇到 preset 不一致先停在维护态对账。
之后按[客户端闭环](client-integration.zh-CN.md#p3a-workflow) apply 或 start/load → 轮询 → 当前 ready → 推理 → unload/stop。
创建成功、Running 和 `/v1/models` 有条目都不证明真实推理成功。

### 可选 N1（n1/1）

仍使用上述 control 实例；在 manager 配置加以下块，并在受控停止窗口重新启动管理器：

```yaml
n1:
  instance: "router"
  roots:
    local_text: "${WORKSTATION_MODEL_ROOT}"
  engine_identity: "<fixed-engine-package-identity>"
  administrator_read_only_placement: true
```

先设置批准的绝对 `WORKSTATION_MODEL_ROOT`，用核实的固定引擎身份替换占位符。
Windows 执行要求 `administrator_read_only_placement=true`：管理员确实负责模型只读投放/避免运行期间改写后才能声明，
该开关不替代 ACL，也不是抗恶意本机写入保证。不要为了让请求成功而虚假确认。
批准根/模型不得含 symlink 或 Windows reparse/junction；不要把含链接的模型目录直接用作 N1 root。

按[配置与权限](control-api.zh-CN.md#n1)由管理员创建单实例 `controller=false` 推理 key，
明确 `can_start=false/can_evict=false`；再 `PUT .../capabilities/{key_id}` 授所需能力。
最小日常 key 为 `discover/service_use/inference`；评测另需 evaluate，recipe 写/发布分别授权。
模型扫描→verify→register 不加载，也不证明可运行；新库不能照抄别库 artifact ID。
按[已有评测与 recipe 操作](client-integration.zh-CN.md#evaluation-recipes)形成或导入本地可解析 recipe，
明确版本和发布状态，再 use → infer → release。首次无 recipe 时不要假设导入就自动发布/切换。

## 5. 本机验收，再开放获准内网访问

分层记录真实结果，不将某层 PASS 外推到下一层：

1. 管理器：网页确有本次 WebUI、管理鉴权正常、未带凭据的管理请求被拒绝；核独立 DB/配置路径。
2. 控制：当前 boot/revision 和 owner 准入正常，操作轮询成功，准确实际模型、ready token/代际匹配。
3. 推理：在已获准的有限请求/超时范围内发送最小合成文本；检查 HTTP、准确响应身份、非空正文、finish_reason，
   SSE 还核完整结束。空正文、只有 reasoning、截断或断流不算完成。
4. 释放：p3a unload/stop 或 N1 release 并轮询清理；owner 暂停能挡住新加载。
   只检查确认归属的后端树，不杀外部程序；manager 可保留。释放成功不承诺整卡显存归零。
5. [资源](resources.md)分整机/进程口径，null/stale/不支持附原因；[测试](deployment-tests.md)会真实加载和生成，
   不能当纯只读 health。本机 GPU 是否实际使用要据引擎和目标环境结果单独验证。

只有获准后，才将 `server.host` 从 loopback 改成批准的私网地址，按既有网络方案放行 **manager** 端口并重启该 manager。
后端保持隔离 loopback，不向客户端暴露实例端口；不默认开放公网、全部网卡或防火墙。
本程序 HTTP 监听本身不提供 TLS；非 loopback 凭据传输使用获准的受保护私网/TLS 入口。
CORS 只约束浏览器，不是网络 ACL；只配确需的 origin/请求头。浏览器核额外响应身份头是否可读，
不能假设默认 expose 已含全部 N1 header，也不能靠 `allowed_origins: ["*"]` 解决鉴权问题。

在另一客户端使用管理员给定 origin、实例名、最小 key 与模型/recipe 版本，照[接入指南](client-integration.zh-CN.md)运行。
`localhost` 在那里指客户端自己，不能仍填 localhost；OpenAI base URL 是 origin + `/v1`，管理/控制是 origin + `/api/v1`。
另一客户端先只读发现，再按授权做一次有界闭环并释放，不给它管理 key，也不让它远程改部署。
本机合成通过不等于这一步已通过；跨设备/私网、Windows/GPU 结果各自记录。

<a id="lifecycle-troubleshooting"></a>

## 6. 基本启停、排错和升级/恢复

- 启动：在固定运行目录、正确配置路径和凭据已注入的会话中运行对应二进制。端口被占用先查已有归属，不再开第二 manager。
- 停止：先按协议结束本轮 use/测试和在途请求并确认释放，再在管理器前台按 Ctrl-C，等待退出/关闭 DB 日志及归属端口释放。
  只停止本轮确认归属的进程；不要 `killall`、按进程名批杀或强制结束不明树。旧普通实例可能按其配置在下次启动恢复；受控实例不自动恢复加载。
- 连不上：查目标 origin、监听地址/端口、批准的网络路径、当前配置加载日志；不是先放开公网/关闭鉴权。
- 401/403：查管理/推理 key 类型、过期、scope、controller 与 capability；owner 操作需明确配置的管理 key。不要换全权 key 绕过。
- 409、paused、recovery_required、旧 token：查原 operation/run、boot/revision 与 owner，不自动恢复或改 key 重跑。
- 引擎启动/加载失败：查固定引擎和依赖库、模型身份/完整性、参数范围及本机资源；保留失败阶段与 cleanup，
  不把启动失败改记为质量差或 0 显存。更多见[排错](troubleshooting.md)。
- SQLite/嵌入 UI 出错：核 CGO/C 编译器、目标架构、WebUI 是否本次构建、数据目录权限；不要通过删 DB 解决。

升级管理器、引擎或恢复已有数据必须另有相应授权。停止写入并确认进程退出后，成套保全旧二进制/运行库与摘要、
实际配置及安全凭据来源、整个状态目录（DB、存在的 WAL/SHM、派生 preset、resource-policy.json 等）、固定引擎与模型身份。
活动 SQLite 的单个主文件拷贝不是一致备份。模型和独立 cache 可能不在 data_dir，另外记录位置与身份，不盲复制或删除。

先在独立副本用候选版本核兼容和自动迁移；启动就可能升级 schema，旧 EXE 配新 DB 不是安全回退。
回退用已核对的一套旧二进制/配置/DB/派生文件，不拼装不同时间的状态，也不自动运行 down migration。
保留失败后像和 owner 暂停状态；新 boot 的旧 operation/ready/use 不续跑，按事实恢复，不重放普通推理。
recipe export/import 是可移植配置，不是完整 DB/凭据/运行状态备份；外来导入仍须本地解析与授权发布。

本文的命令为目标机将来执行的部署步骤。仓库离线链接/示例/站点构建通过，仅证明说明可读且与源码静态对应，
不代表已取得机器权限、已安装依赖、已做新机/模型/GPU/网络验收或已发布定制产物。
