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
EXPOSE 8000
CMD ["alertflow-agent"]
