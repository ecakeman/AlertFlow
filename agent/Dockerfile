FROM python:3.12-slim
ENV PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple
ENV PIP_TRUSTED_HOST=mirrors.aliyun.com
WORKDIR /app
COPY pyproject.toml schema.sql ./
COPY src ./src
RUN pip install --no-cache-dir -e .
EXPOSE 8000
CMD ["alertflow-agent"]
