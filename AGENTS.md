# 仓库任务入口

本仓库是 llamactl 定制源码：Go 管理器、React WebUI、SQLite；复用官方推理后端。

- 了解功能与边界：[README.md](README.md)。
- 在新 Windows/Linux 工作站构建、初始化和验证：[工作站部署指南](docs/workstation-deployment.zh-CN.md)。
- 调用已部署服务：[客户端接入与使用](docs/client-integration.zh-CN.md)，字段查 [控制/N1 契约](docs/control-api.zh-CN.md)和 [OpenAPI](docs/control-openapi.yaml)。
- 开发、测试与契约维护：[CONTRIBUTING.md](CONTRIBUTING.md)。其余专题从 [docs/index.md](docs/index.md)进入。

按本次任务授权工作；开始编辑前检查 Git 状态，保全已有修改。必要本地可逆操作可连续完成。
部署/替换现有服务、迁移现有数据库、凭证与网络/驱动/自启变更、删除和远程发布须有相应明确许可。
不要把启动管理器当只读检查：它会建目录、迁移数据库，且可能恢复普通实例。
只停止确认归属的进程，不按名称清理其他服务；不将真实地址、凭据、模型路径配置、私人素材和日志写入仓库。
区分普通实例、仅 controlled 准入和 control/N1 协议；不以普通实例的自动加载或旧 UI 操作替代受控流程。
HTTP 接受、进程 Running、模型 ready、有效推理和资源释放分别验证；离线/合成测试不代表新机或 GPU 实测。
这些公开说明不依赖私人建设记录；本地额外约定不构成新机器部署的前置资料。
