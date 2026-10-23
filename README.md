# llamactl

![Build and Release](https://github.com/lordmathis/llamactl/actions/workflows/release.yaml/badge.svg) ![Go Tests](https://github.com/lordmathis/llamactl/actions/workflows/go_test.yaml/badge.svg) ![WebUI Tests](https://github.com/lordmathis/llamactl/actions/workflows/webui_test.yaml/badge.svg) ![User Docs](https://github.com/lordmathis/llamactl/actions/workflows/docs.yaml/badge.svg)

**Unified management and routing for llama.cpp, MLX and vLLM models with web dashboard.**

📚 **[Full Documentation →](https://llamactl.org)**

## 定制版阅读入口（中文）

本 fork 复用官方推理后端，提供模型/实例管理、有限参数应用与就绪确认、主人暂停与归属释放、
CPU/RAM/GPU/VRAM 观测，以及单配置有界测试和可选 N1 模型目录、持久评测/recipe、直接服务使用。
控制/N1 当前面向一个本机原生 llama.cpp Router，不是通用调度器或业务评分系统。
上游仍有 remote-node 管理能力；这不等于本机定制协议支持跨节点调度或统一 GPU 池。

- 新工作站：[Windows/Linux 部署指南](docs/workstation-deployment.zh-CN.md)（固定源码、CGO/WebUI、独立状态与鉴权、本机到内网验证）。
- 调用现有服务：[客户端接入与使用](docs/client-integration.zh-CN.md)。
- 开发与维护：[CONTRIBUTING.md](CONTRIBUTING.md)；Agent 首读 [AGENTS.md](AGENTS.md)。

从[客户端接入与使用](docs/client-integration.zh-CN.md)开始：区分上游通用 API、
有限控制 `p3a/1` 与可选 N1 扩展；源码/合成验证不等于目标环境已部署或业务质量通过。
契约见[有限控制与 N1](docs/control-api.zh-CN.md)、[OpenAPI](docs/control-openapi.yaml)；
另见[资源观测](docs/resources.md)、[主人暂停与释放](docs/resource-policy.md)、
[单配置测试](docs/deployment-tests.md)及[N1 客户端/评测工具](tools/evaluator/README.md)。
以下为上游功能与部署说明；受控实例不要照普通 Quick Start 用旧 UI/原生接口启动、改配置或自动加载。
本地文档链接不表示定制文档已发布到上游网站。

![Dashboard Screenshot](docs/images/dashboard.png)

## Features

**🚀 Easy Model Management**
- **Built-in Model Downloader**: Download GGUF and Safetensors models directly from HuggingFace for llama.cpp, vLLM, and MLX backends
- **Dynamic Multi-Model Instances**: llama.cpp router mode - serve multiple models from a single instance with on-demand loading
- **Smart Resource Management**: Automatic idle timeout, LRU eviction, instance groups with per-group limits, and configurable instance limits
- **Web Dashboard**: Modern React UI for managing instances, monitoring health, and viewing logs

**🔗 Flexible Integration**
- **API Compatible**: OpenAI chat completions and resources endpoints, Anthropic messages endpoint (depending on backend) - route requests to different models by instance name
- **Multi-Backend Support**: Native support for llama.cpp, MLX (Apple Silicon optimized), and vLLM, plus any OpenAI-compatible server via configurable custom backends
- **Docker Ready**: Run backends in containers with full GPU support

**🌐 Distributed Deployment**
- **Remote Instances**: Deploy instances on remote hosts
- **Central Management**: Manage everything from a single dashboard with automatic routing  

## Quick Start

1. Install a backend (llama.cpp, MLX, or vLLM) - see [Prerequisites](#prerequisites) below
2. [Download llamactl](#installation) for your platform
3. Run `llamactl` and open http://localhost:8080
4. Create an instance and start inferencing!

## Prerequisites

### Backend Dependencies

**For llama.cpp backend:**
You need `llama-server` from [llama.cpp](https://github.com/ggml-org/llama.cpp) installed:

```bash
# Homebrew (macOS)
brew install llama.cpp

# Or build from source - see llama.cpp docs
# Or use Docker - no local installation required
```

**For MLX backend (macOS only):**
You need MLX-LM installed:

```bash
# Install via pip (requires Python 3.8+)
pip install mlx-lm

# Or in a virtual environment (recommended)
python -m venv mlx-env
source mlx-env/bin/activate
pip install mlx-lm
```

**For vLLM backend:**
You need vLLM installed:

```bash
# Install via pip (requires Python 3.8+, GPU required)
pip install vllm

# Or in a virtual environment (recommended)
python -m venv vllm-env
source vllm-env/bin/activate
pip install vllm

# Or use Docker - no local installation required
```

### Docker Support

llamactl can run backends in Docker containers, eliminating the need for local backend installation:

```yaml
backends:
  llama-cpp:
    docker:
      enabled: true
  vllm:
    docker:
      enabled: true
```

## Installation

### Option 1: Download Binary (Recommended)

以下下载/镜像来自上游，不保证包含本 fork 的 control/N1 定制；定制部署请用上方工作站指南，
核维护者指定的完整提交/候选清单和产物摘要，不从上游 latest 推定定制产物已发布。

```bash
# Linux/macOS - Get latest version and download
LATEST_VERSION=$(curl -s https://api.github.com/repos/lordmathis/llamactl/releases/latest | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
curl -L https://github.com/lordmathis/llamactl/releases/download/${LATEST_VERSION}/llamactl-${LATEST_VERSION}-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m).tar.gz | tar -xz
sudo mv llamactl /usr/local/bin/

# Or download manually from the releases page:
# https://github.com/lordmathis/llamactl/releases/latest

# Windows - Download from releases page
```

### Option 2: Docker (No local backend installation required)

```bash
# Clone repository and build Docker images
git clone https://github.com/lordmathis/llamactl.git
cd llamactl
mkdir -p data/llamacpp data/vllm models

# Build and start llamactl with llama.cpp CUDA backend
docker-compose -f docker/docker-compose.yml up llamactl-llamacpp -d

# Build and start llamactl with vLLM CUDA backend
docker-compose -f docker/docker-compose.yml up llamactl-vllm -d

# Build from source using multi-stage build
docker build -f docker/Dockerfile.source -t llamactl:source .
```

**Note:** Dockerfiles are configured for CUDA. Adapt base images for other platforms (CPU, ROCm, etc.).

### Option 3: Build from Source
Requires Go 1.26 (see go.mod), Node.js 22.12+ and a target C compiler with CGO enabled for SQLite.
The clone below is upstream; for this fork use the maintainer-provided fixed source instead.
```bash
git clone https://github.com/lordmathis/llamactl.git
cd llamactl
cd webui && npm ci && npm run build && cd ..
CGO_ENABLED=1 go build -o llamactl ./cmd/server
```

## Usage

1. Open http://localhost:8080
2. Click "Create Instance"
3. Choose backend type (llama.cpp, MLX, or vLLM)
4. Configure your model and options (ports and API keys are auto-assigned)
5. Start the instance and use it with any OpenAI-compatible client

## Configuration

下面保留上游通用配置示例（含通配监听/CORS）。新工作站请先用
[loopback 与独立状态配置](docs/workstation-deployment.zh-CN.md#local-config)，
不要将这些默认值直接当内网部署方案；受控实例也不使用普通实例的自动加载/驱逐流程。

llamactl works out of the box with sensible defaults. Configuration is loaded with the following precedence:

```
Defaults < .env file < Configuration file < LLAMACTL_* environment variables
```

Config files support `${VAR}` and `${VAR:-default}` placeholders, and a `.env` file is automatically loaded.

```yaml
server:
  host: "0.0.0.0"                # Server host to bind to
  port: 8080                     # Server port to bind to
  allowed_origins: ["*"]         # Allowed CORS origins (default: all)
  allowed_headers: ["*"]         # Allowed CORS headers (default: all)
  enable_swagger: false          # Enable Swagger UI for API docs

backends:
  llama-cpp:
    command: "llama-server"
    args: []
    environment: {}               # Environment variables for the backend process
    docker:
      enabled: false
      image: "ghcr.io/ggml-org/llama.cpp:server"
      args: ["run", "--rm", "--network", "host", "--gpus", "all", "-v", "~/.local/share/llamactl/llama.cpp:/root/.cache/llama.cpp"]
      environment: {}             # Environment variables for the container

  vllm:
    command: "vllm"
    args: ["serve"]
    environment: {}               # Environment variables for the backend process
    docker:
      enabled: false
      image: "vllm/vllm-openai:latest"
      args: ["run", "--rm", "--network", "host", "--gpus", "all", "--shm-size", "1g", "-v", "~/.local/share/llamactl/huggingface:/root/.cache/huggingface"]
      environment: {}             # Environment variables for the container

  mlx:
    command: "mlx_lm.server"
    args: []
    environment: {}               # Environment variables for the backend process

data_dir: ~/.local/share/llamactl  # Main data directory (database, instances, logs), default varies by OS

instances:
  port_range: [8000, 9000]                        # Port range for instances
  logs_dir: ~/.local/share/llamactl/logs          # Logs directory (platform dependent)
  auto_create_dirs: true                          # Auto-create data/config/logs dirs if missing
  max_instances: -1                               # Max instances (-1 = unlimited)
  max_running_instances: -1                       # Max running instances (-1 = unlimited)
  enable_lru_eviction: true                       # Enable LRU eviction for idle instances
  default_idle_timeout: 30                        # Default idle timeout in minutes (0 = no timeout)
  default_auto_restart: true                      # Auto-restart new instances by default
  default_max_restarts: 3                         # Max restarts for new instances
  default_restart_delay: 5                        # Restart delay (seconds) for new instances
  default_on_demand_start: true                   # Default on-demand start setting
  on_demand_start_timeout: 120                    # Default on-demand start timeout in seconds
  group_limits: {}                                # Per-group instance limits (e.g., large: 1, small: 3)
  timeout_check_interval: 5                       # Idle instance timeout check in minutes
  log_rotation_enabled: true                      # Enable log rotation (default: true)
  log_rotation_max_size: 100                      # Max log file size in MB before rotation (default: 100)
  log_rotation_compress: false                    # Compress rotated log files (default: false)

database:
  path: ~/.local/share/llamactl/llamactl.db  # Database file path (platform dependent)
  max_open_connections: 25                   # Maximum open database connections
  max_idle_connections: 5                    # Maximum idle database connections
  connection_max_lifetime: 5m                # Connection max lifetime

auth:
  require_inference_auth: true   # Require auth for inference endpoints, API keys are created in web UI
  require_management_auth: true  # Require auth for management endpoints
  management_keys: []            # Keys for management endpoints
```

For detailed configuration options including environment variables, file locations, and advanced settings, see the [Configuration Guide](docs/configuration.md).

## License

MIT License - see [LICENSE](LICENSE) file.
