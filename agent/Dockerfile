ARG PYTHON_IMAGE=python:3.12-slim
ARG PIP_INDEX_URL=https://pypi.org/simple
FROM ${PYTHON_IMAGE}
ARG PIP_INDEX_URL
ENV PIP_INDEX_URL=${PIP_INDEX_URL}
WORKDIR /app
COPY pyproject.toml schema.sql ./
COPY diagnostics.example.yaml /diagnostics/diagnostics.yaml
COPY src ./src
RUN pip install --no-cache-dir -e .
EXPOSE 8000
CMD ["alertflow-agent"]
