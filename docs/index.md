# Llamactl Documentation

Welcome to the Llamactl documentation!  

## 四个阅读入口

本 fork 复用官方后端，增加有限参数/就绪控制、主人暂停与归属释放、整机/进程资源观测，
以及单配置测试和可选 N1 inventory、持久评测/recipe 与 service-use。
本机定制控制限一个原生 llama.cpp Router；不是业务评分平台或统一 GPU 池。
上游 remote-node 管理仍保留，但不能据此推定 control/N1 支持跨节点调度。

1. 项目说明：本页；功能摘要来自仓库 README。
2. [工作站部署](workstation-deployment.zh-CN.md)：Windows/Linux 构建、独立配置与鉴权、本机及获准内网验证、必要启停/排错/升级说明。
3. [使用与接口](client-integration.zh-CN.md)：调用闭环与以下专题；不要求私人建设资料。
4. 开发说明：仓库根 `CONTRIBUTING.md`；Agent 导航在根 `AGENTS.md`（源码文件，不是站内页面）。

首读[客户端接入与使用](client-integration.zh-CN.md)：上游通用 API、有限控制 `p3a/1`
与可选 N1 扩展分开；按目标环境的实际部署、配置与权限选择，不凭文档标题推定可用。
契约：[有限控制与 N1](control-api.zh-CN.md)、[OpenAPI](control-openapi.yaml)。
专题：[资源观测](resources.md)、[主人暂停与释放](resource-policy.md)、
[单配置测试](deployment-tests.md)、[N1 客户端/评测工具](evaluator-client.md)。
以下功能介绍与 Quick Start 主要描述上游普通实例，不能替代受控实例调用流程。

![Dashboard Screenshot](images/dashboard.png)

## What is Llamactl?

**{{HEADLINE}}**

## Features

{{FEATURES}}  

## Quick Links

- [Installation Guide](installation.md) - Get Llamactl up and running
- [定制版工作站部署](workstation-deployment.zh-CN.md) - 固定版本的 Windows/Linux 源码构建与运行
- [Configuration Guide](configuration.md) - Detailed configuration options
- [Quick Start](quick-start.md) - Your first steps with Llamactl
- [Authentication](authentication.md) - API keys, permissions, and SSO login
- [Managing Instances](managing-instances.md) - Instance lifecycle management
- [Managing Models](managing-models.md) - Download and manage models from HuggingFace
- [Custom Backends](custom-backends.md) - Manage inference servers without native support
- [API Reference](api-reference.md) - Complete API documentation


## Getting Help

If you need help or have questions:

- Check the [Troubleshooting](troubleshooting.md) guide
- Visit the [GitHub repository](https://github.com/lordmathis/llamactl)
- Review the [Configuration Guide](configuration.md) for advanced settings

## License

MIT License - see the [LICENSE](https://github.com/lordmathis/llamactl/blob/main/LICENSE) file.
