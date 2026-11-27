FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /app

COPY pyproject.toml ./
COPY src ./src
RUN pip install --upgrade pip && pip install .

EXPOSE 8001

CMD ["sh", "-c", "uvicorn nexusflow_agents.main:app --host 0.0.0.0 --port ${AGENT_HUB_PORT:-8001}"]
