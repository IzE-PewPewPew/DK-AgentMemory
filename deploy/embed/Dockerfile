# syntax=docker/dockerfile:1

FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HF_HUB_DISABLE_TELEMETRY=1

RUN useradd --create-home --uid 10002 embed

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .

RUN chown -R embed:embed /app
USER embed

EXPOSE 8091

# One worker. The model is loaded per worker, so a second one doubles the
# memory for no throughput gain on the single core these usually run on.
CMD ["uvicorn", "app:api", "--host", "0.0.0.0", "--port", "8091", "--workers", "1"]
