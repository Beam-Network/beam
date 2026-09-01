# Build from the repository root, not from this directory:
#
#   docker build -f actors/worker/Dockerfile -t beam-worker:latest .
#

FROM python:3.11-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    pkg-config \
    libssl-dev \
    && rm -rf /var/lib/apt/lists/*

COPY pyproject.toml README.md /app/
COPY beam /app/beam/
COPY actors /app/actors/

# Keep bittensor on the 7-9 series used by this worker entrypoint.
# async-substrate-interface 2.x brings cyscale conflicts with scalecodec, so pin the 1.x branch.
RUN printf "async-substrate-interface>=1.5.6,<2\n" > /tmp/pip-constraints.txt \
    && pip install --no-cache-dir --upgrade pip \
    && pip install --no-cache-dir . \
    && pip install --no-cache-dir -c /tmp/pip-constraints.txt --upgrade "bittensor>=7.0.0,<10" \
    && python -c "import bittensor as bt; v=bt.__version__; print('bittensor OK', v)"

WORKDIR /app/actors/worker

ENTRYPOINT ["python", "worker.py"]
CMD ["--subtensor.network", "finney"]
