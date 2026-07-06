# agent-kit 官方执行运行环境示例:给 impl/exec/docker 沙箱用的基础镜像。
# 预装脚本运行时与常用库,技能(pdf 等)的依赖在镜像层一次性解决——
# 不碰宿主、不靠模型临场 pip install。
#
#   docker build -t agent-kit-runtime:latest -f examples/exec-runtime/Dockerfile .
#
# 配置里引用:
#   exec:
#     default_sandbox: docker
#     sandbox_config: {image: "agent-kit-runtime:latest", network: none, memory: 512m}
#     require_sandbox: true          # 生产:禁止宿主直跑
#
# 沙箱以 `docker run --rm -i --network none --read-only ...` 一次性加固容器
# 执行脚本(python3 -c / node -e)。按需在此追加运行时/库。
FROM python:3.12-slim
RUN pip install --no-cache-dir pypdf pdfplumber
RUN apt-get update \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /tmp
