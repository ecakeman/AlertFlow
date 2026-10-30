ARG PYTHON_IMAGE=python:3.12-slim
ARG PIP_INDEX_URL=https://pypi.org/simple
FROM ${PYTHON_IMAGE}
ARG PIP_INDEX_URL
ENV PIP_INDEX_URL=${PIP_INDEX_URL}
WORKDIR /app
COPY pyproject.toml uv.lock schema.sql ./
COPY diagnostics.example.yaml /diagnostics/diagnostics.yaml
COPY src ./src
RUN pip install --no-cache-dir uv \
 && uv export --frozen --no-dev --no-emit-project -o /tmp/requirements.txt \
 && uv pip install --system --no-cache -r /tmp/requirements.txt \
 && uv pip install --system --no-cache --no-deps -e .
# 构建上下文的文件权限可能是 600（常见于以 root/编辑器创建的源码）；editable 安装后
# 非 root 用户仍从 /app/src 读取，必须放开读权限，否则容器只能以 root 运行。
RUN chmod -R a+rX /app/src
EXPOSE 8000
CMD ["alertflow-agent"]
