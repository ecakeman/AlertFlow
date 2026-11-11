# Python Main Agent：构建阶段根据 pyproject.toml / uv.lock 在镜像内安装依赖，
# 不依赖宿主机 .venv；最终镜像使用非 root 用户（uid 10001）。
FROM python:3.12-slim-bookworm

# 默认使用官方 PyPI；国内构建可 --build-arg PIP_INDEX_URL=<镜像源>/simple 覆盖。
ARG PIP_INDEX_URL=https://pypi.org/simple
ENV PIP_INDEX_URL=${PIP_INDEX_URL} \
    UV_DEFAULT_INDEX=${PIP_INDEX_URL} \
    UV_PYTHON_DOWNLOADS=never \
    UV_LINK_MODE=copy \
    UV_COMPILE_BYTECODE=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

RUN pip install --no-cache-dir uv

WORKDIR /app
COPY pyproject.toml uv.lock ./
# --no-install-project：先装第三方依赖，业务代码随后 COPY，改源码不会重建依赖层。
RUN uv sync --frozen --no-dev --no-install-project

COPY src ./src

RUN groupadd --system --gid 10001 alertflow \
    && useradd --system --uid 10001 --gid alertflow --create-home --home-dir /home/alertflow --shell /usr/sbin/nologin alertflow

# 虚拟环境在 PATH 中，CMD 的 python 即 /app/.venv/bin/python；源码通过 PYTHONPATH 引入。
ENV PATH="/app/.venv/bin:${PATH}" \
    PYTHONPATH=/app/src

USER 10001:10001
EXPOSE 9000 8000
# 与现有健康检查一致：使用标准库访问 FastAPI /health/live，不额外安装 curl。
HEALTHCHECK --interval=5s --timeout=3s --start-period=15s --retries=20 \
    CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000/health/live', timeout=2)" || exit 1
CMD ["python", "-m", "alertflow_agent"]
