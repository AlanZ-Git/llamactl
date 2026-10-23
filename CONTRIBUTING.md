# 参与 Llamactl 开发

感谢参与 Llamactl 开发！本文介绍开发环境、必要检查和贡献流程。

## 开发环境

### 前置条件

- Go 1.26（`go.mod` 指定 `1.26.0`；可使用兼容的补丁版本）
- Node.js 22.12+ 与 npm（遵循 `webui/package-lock.json`；发布工作流使用 Node 22）
- 目标平台的 C 编译器，并设置 `CGO_ENABLED=1`，以支持 `github.com/mattn/go-sqlite3`
- 仅在获准的真实引擎测试中使用固定版本的 `llama-server`；单元/契约测试使用测试夹具

Windows/Linux 构建与独立启动见[工作站部署指南](docs/workstation-deployment.zh-CN.md)。
先构建 WebUI，再构建 Go：`webui/webui.go` 嵌入 `dist/*`；旧 dist 不代表本次源码构建，CGO=0 的产物也不是可用的 SQLite 部署。

### 开始开发

1. **取得指定源码**
   使用维护者提供的 fork 和完整提交 ID，或带有已核验文件清单的候选源码。
   记录 `git rev-parse HEAD` 与 `git status --short`，保全已有修改。
   上游 latest/镜像不能证明包含本 fork 的 controlled/N1 能力。

2. **安装依赖**
   ```bash
   # Go 依赖
   go mod download
   
   # 前端依赖
   cd webui && npm ci && cd ..
   ```

3. **启动开发环境**

   先按工作站指南明确设置 `LLAMACTL_CONFIG_PATH`、独立的绝对数据/缓存路径、loopback 监听与安全注入的管理凭据。
   `go run` 不是只读检查：它会初始化/迁移 SQLite，并可能恢复普通实例。不要为了快速检查而指向日常状态目录。
   先用 `cd webui && npm run build && cd ..` 构建嵌入的 UI。
   ```bash
   # 启动后端服务
   go run ./cmd/server
   ```
   使用示例端口时，后端地址为 `http://localhost:8080`。
   
   ```bash
   # 在另一终端启动前端开发服务
   cd webui && npm run dev
   ```
   使用默认开发端口时，UI 地址为 `http://localhost:5173`。

4. **常用开发命令**
   ```bash
   # 后端
   go test ./... -v                    # 运行测试
   go test -race ./... -v              # 启用竞态检测
   go vet ./...                       # 静态检查；格式化仅限本次修改的文件
   
   # 前端（在 webui/ 目录执行）
   npm run test:run                    # 单次运行测试
   npm run test                        # 监听模式测试
   npm run type-check                  # TypeScript 类型检查
   npm run lint:fix                    # 检查并修复 lint 问题
   ```

### 模块入口

| 路径 | 职责 |
|---|---|
| `cmd/server/main.go`, `pkg/config/` | 启动、配置/环境变量优先级与路径 |
| `pkg/server/` | HTTP 路由、鉴权、control/N1 处理器、代理与契约测试 |
| `pkg/manager/` | 实例生命周期、控制/测试、N1 预约/执行/配方 |
| `pkg/instance/`, `pkg/backends/` | 准入、选项/preset、进程归属、ready 身份与 argv |
| `pkg/database/` | SQLite 存储与嵌入迁移，含 N1 004 |
| `pkg/resourcepolicy/`, `pkg/resources/` | owner 暂停/释放与资源采样 |
| `pkg/models/` | 下载与 N1 模型清单/内容身份 |
| `webui/` | React UI、Vitest 测试与嵌入构建产物 |
| `tools/evaluator/` | 仅使用标准库的 N1 CLI；[唯一用法来源](tools/evaluator/README.md) |

Options/SQLite 是实际部署配置的权威来源，preset.ini 是派生产物。Recipe 版本是不可变的配置/证据，
不是第二套可变的运行配置库。文件、DB 和进程不属于同一事务，须暴露并保留失败阶段。
代码和测试中要区分普通实例、仅 controlled 准入与启用协议的 control/N1。
全树格式化/lint 修复会写文件；优先定向格式化，避免混入无关改动。

## 提交 Pull Request 前

### 必要检查

代码修改在开发中运行相关测试，提交前执行以下检查。
纯文档修改检查受影响的链接/锚点/导航/示例及源码对应，不要求无关的全量回归或真实模型运行。
记录实际命令、结果和未验证边界。
新源码树须先构建前端，再执行导入嵌入 WebUI 包的 Go 测试、vet 或构建。

1. **后端**
   ```bash
   go test ./... -v
   go test -race ./... -v
   go vet ./...
   # 先完成下方前端构建；Unix 示例，产物放在仓库外
   CGO_ENABLED=1 go build -o ../llamactl-build/llamactl ./cmd/server
   ```

2. **前端**
   ```bash
   cd webui
   npm run test:run
   npm run type-check
   npm run build
   ```

Linux 竞态/mock 测试及交叉编译不证明 Windows/SQLite 运行、GPU 加载、ready 或有效推理通过。
UI 修改还需实际浏览器验证。真实引擎测试、网络/生产/凭据变更及现有 DB 迁移需相应授权；
调用在线 CLI 评测不是只读检查。

### API 文档

同步维护两层 API：

- 普通 API：相关 `pkg/server/` 处理器注释及 `docs/docs.go`、`docs/swagger.json`、`docs/swagger.yaml`。
- 受控/N1：`docs/control-api.zh-CN.md`、`docs/control-openapi.yaml`、客户端/受影响功能文档、
  `pkg/server/*openapi_test.go`，以及相应的鉴权/校验/生命周期测试。

覆盖 missing/null/0/false、未知字段、权限、boot/revision、ready/use 身份、owner 暂停、不重放，
以及相互独立的执行/清理结果。不要把清理失败改记为执行成功。
需要重新生成时，使用 `go.mod` 固定的 Swag 版本，不跟随 latest：

```bash
# 按需安装 swag
go install github.com/swaggo/swag/cmd/swag@v1.16.5

# 更新相关 pkg/server/handlers_*.go 文件中的注释
# 然后重新生成文档
swag init -g cmd/server/main.go
```

审查生成差异；Swag 不生成 `control-openapi.yaml`。
定向合成检查包括 `go test ./pkg/server -run 'OpenAPI'` 与
`python3 -m unittest discover -s tools/evaluator/tests`；选择受影响的检查，不以在线 CLI 调用代替。

## Pull Request 约定

### Pull Request 标题
标题使用以下类型前缀：
- `feat:`：新功能
- `fix:`：错误修复
- `docs:`：文档修改
- `test:`：新增或修改测试
- `refactor:`：代码重构

### 提交流程
1. 遵循已约定的 fork 流程：`main` 跟踪上游，功能分支经 `dev` 集成并验收到 `stable`；不直接在 `main`/`stable` 开发
2. 按编码规范修改
3. 执行上文必要检查
4. 按需更新文档
5. 取得相应授权后才 commit/push/PR（开发授权不等于远程发布许可），提交时提供：
   - 清晰的改动说明
   - 相关 issue 引用
   - UI 修改的截图

## 代码风格与测试

### 测试策略
- 后端使用 Go 内置测试框架
- 前端使用 Vitest 与 React Testing Library
- 开发中经常运行相关测试
- 新功能与错误修复补充测试

### Go
- 遵循 Go 标准格式（`go fmt`）
- 使用有意义的变量名和函数名
- 为导出函数和类型添加注释
- 恰当处理错误

### TypeScript/React
- 严格使用 TypeScript，尽量避免 `any`
- 遵循 React hooks 的最佳实践
- 使用有意义的组件名和变量名
- 优先使用函数组件而非类组件

## 文档开发

本项目使用 MkDocs，文档开发按以下方式进行。

### 准备文档环境

```bash
# 使用临时 venv，不修改共享/系统 Python
python3 -m venv /tmp/llamactl-docs-venv
/tmp/llamactl-docs-venv/bin/python -m pip install -r docs-requirements.txt
```

### 开发流程

```bash
# 本地预览文档
/tmp/llamactl-docs-venv/bin/mkdocs serve
```
默认预览地址为 http://localhost:8000。

```bash
# 构建静态文档站点
/tmp/llamactl-docs-venv/bin/mkdocs build --site-dir /tmp/llamactl-docs-site
```
站点输出到指定临时目录。单独审查警告；非严格构建成功不证明所有链接/平台说明有效（CI 使用 `--strict`）。
Windows 使用独立 venv 中的 `Scripts/python.exe`/`Scripts/mkdocs.exe`。

### 文档结构

- `docs/`：文档正文（Markdown 文件）
- `mkdocs.yml`：MkDocs 配置
- `docs-requirements.txt`：文档所需 Python 依赖

### 新增文档

新增文档时：

1. 在合适的 `docs/` 子目录创建 Markdown 文件
2. 更新 `mkdocs.yml` 导航
3. 校验链接/锚点与示例，构建时保留既有 `readme_sync.py` 和 `fix_line_endings.py` hook
4. 核验明确的公开候选白名单，确保没有私有依赖；仅在获准后发布

产品保持四入口：README/index、工作站部署、既有客户端/API/功能文档，以及本文。
不要重复维护 CLI 参数：`docs/evaluator-client.md` 同源嵌入 `tools/evaluator/README.md`。
真实主机/账号/设备坐标、凭据、原始请求、私人素材和日志不得进入公开文件。
本地建设/进度记录不是构建或使用导出仓库的前置条件。

### 文档发布

仓库已有面向上游的 Pages 工作流，不代表本 fork 的文档/二进制已经发布。
获准推送前核对目标远端、完整白名单与历史；本地文档编辑不附带部署。

## 获取帮助

- 查阅已有 [issues](https://github.com/lordmathis/llamactl/issues)
- 从 [README.md](README.md) 查找用法文档
- 参考既有代码中的模式与约定

感谢为 Llamactl 作出贡献！