"""
Prometheus Metrics for BEAM Orchestrator

Provides comprehensive metrics for monitoring:
- Request latencies and counts
- Worker statistics
- Transfer metrics
- Proof aggregation metrics
- System health
"""

import asyncio
import logging
import time
from typing import Callable, Optional

from fastapi import Request, Response
from starlette.middleware.base import BaseHTTPMiddleware

try:
    from prometheus_client import (
        CONTENT_TYPE_LATEST,
        Counter,
        Gauge,
        Histogram,
        Info,
        generate_latest,
    )

    PROMETHEUS_AVAILABLE = True
except ImportError:
    PROMETHEUS_AVAILABLE = False

    # Create stub classes for when prometheus_client is not installed
    class Counter:
        def __init__(self, *args, **kwargs):
            pass

        def labels(self, *args, **kwargs):
            return self

        def inc(self, *args, **kwargs):
            pass

    class Gauge:
        def __init__(self, *args, **kwargs):
            pass

        def labels(self, *args, **kwargs):
            return self

        def set(self, *args, **kwargs):
            pass

        def inc(self, *args, **kwargs):
            pass

        def dec(self, *args, **kwargs):
            pass

    class Histogram:
        def __init__(self, *args, **kwargs):
            pass

        def labels(self, *args, **kwargs):
            return self

        def observe(self, *args, **kwargs):
            pass

    class Info:
        def __init__(self, *args, **kwargs):
            pass

        def info(self, *args, **kwargs):
            pass

    def generate_latest(*args, **kwargs):
        return b""

    CONTENT_TYPE_LATEST = "text/plain"


logger = logging.getLogger(__name__)


# =============================================================================
# Metric Definitions
# =============================================================================

# Service info
SERVICE_INFO = Info(
    "beam_orchestrator",
    "BEAM Orchestrator service information",
)

# Request metrics
REQUEST_COUNT = Counter(
    "beam_http_requests_total",
    "Total HTTP requests",
    ["method", "endpoint", "status"],
)

REQUEST_LATENCY = Histogram(
    "beam_http_request_duration_seconds",
    "HTTP request duration in seconds",
    ["method", "endpoint"],
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0),
)

REQUEST_IN_PROGRESS = Gauge(
    "beam_http_requests_in_progress",
    "Number of HTTP requests currently being processed",
    ["method", "endpoint"],
)

# Worker metrics
WORKERS_TOTAL = Gauge(
    "beam_workers_total",
    "Total number of registered workers",
    ["status"],  # active, pending, suspended
)

WORKERS_BY_TIER = Gauge(
    "beam_workers_by_tier",
    "Number of workers by tier",
    ["tier"],  # Probation, Standard, Premium
)

WORKER_TRUST_SCORE = Histogram(
    "beam_worker_trust_score",
    "Distribution of worker trust scores",
    buckets=(0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0),
)

WORKER_BANDWIDTH = Histogram(
    "beam_worker_bandwidth_mbps",
    "Distribution of worker bandwidth (Mbps)",
    buckets=(10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000),
)

# Task metrics
TASKS_TOTAL = Counter(
    "beam_tasks_total",
    "Total tasks processed",
    ["type", "status"],  # type: relay, store; status: completed, failed
)

TASKS_IN_PROGRESS = Gauge(
    "beam_tasks_in_progress",
    "Number of tasks currently in progress",
)

TASK_DURATION = Histogram(
    "beam_task_duration_seconds",
    "Task processing duration in seconds",
    ["type"],
    buckets=(0.1, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0, 120.0, 300.0),
)

# Transfer metrics
TRANSFERS_TOTAL = Counter(
    "beam_transfers_total",
    "Total file transfers",
    ["destination_type", "status"],  # destination: direct, storage, relay, stream, webhook
)

TRANSFER_BYTES = Counter(
    "beam_transfer_bytes_total",
    "Total bytes transferred",
    ["direction"],  # ingress, egress
)

TRANSFER_THROUGHPUT = Histogram(
    "beam_transfer_throughput_mbps",
    "Transfer throughput in Mbps",
    buckets=(10, 25, 50, 100, 250, 500, 1000, 2500),
)

# Epoch metrics
EPOCH_NUMBER = Gauge(
    "beam_current_epoch",
    "Current epoch number",
)

EPOCH_TASKS = Gauge(
    "beam_epoch_tasks",
    "Tasks completed in current epoch",
)

EPOCH_BYTES = Gauge(
    "beam_epoch_bytes",
    "Bytes transferred in current epoch",
)

# Rate limiting metrics
RATE_LIMIT_HITS = Counter(
    "beam_rate_limit_hits_total",
    "Total rate limit violations",
    ["type"],  # per_minute, per_second, burst
)

BLOCKED_CLIENTS = Gauge(
    "beam_blocked_clients",
    "Number of currently blocked clients",
)

# System metrics
MEMORY_USAGE = Gauge(
    "beam_memory_usage_bytes",
    "Memory usage in bytes",
)

CPU_USAGE = Gauge(
    "beam_cpu_usage_percent",
    "CPU usage percentage",
)

UPTIME = Gauge(
    "beam_uptime_seconds",
    "Service uptime in seconds",
)

# BeamCore control path via NATS control
BEAMCORE_UPSTREAM_DEGRADED = Gauge(
    "beam_beamcore_upstream_degraded",
    "1 while the BeamCore NATS control connection is degraded",
)

BEAMCORE_UPSTREAM_DOWN_EVENTS = Counter(
    "beam_beamcore_upstream_down_events_total",
    "Times the BeamCore NATS control connection reported upstream_down",
)


# =============================================================================
# Metrics Collector
# =============================================================================


class MetricsCollector:
    """
    Centralized metrics collection for the Orchestrator.

    Provides methods to update metrics and a background task to
    periodically collect system metrics.
    """

    def __init__(self, orchestrator=None):
        self.orchestrator = orchestrator
        self._start_time = time.time()
        self._collection_task: Optional[asyncio.Task] = None

        # Initialize service info
        if PROMETHEUS_AVAILABLE:
            SERVICE_INFO.info(
                {
                    "version": "0.1.0",
                    "service": "orchestrator",
                    "network": "beam",
                }
            )

    def set_orchestrator(self, orchestrator) -> None:
        """Set the orchestrator instance for metrics collection."""
        self.orchestrator = orchestrator

    async def start(self) -> None:
        """Start background metrics collection."""
        if self._collection_task is None:
            self._collection_task = asyncio.create_task(self._collection_loop())
            logger.info("Metrics collection started")

    async def stop(self) -> None:
        """Stop background metrics collection."""
        if self._collection_task:
            self._collection_task.cancel()
            try:
                await self._collection_task
            except asyncio.CancelledError:
                pass
            self._collection_task = None
            logger.info("Metrics collection stopped")

    async def _collection_loop(self) -> None:
        """Periodically collect system and orchestrator metrics."""
        while True:
            try:
                await asyncio.sleep(15)  # Collect every 15 seconds
                await self._collect_metrics()
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Error collecting metrics: {e}")

    async def _collect_metrics(self) -> None:
        """Collect current metrics from orchestrator and system."""
        # Uptime
        UPTIME.set(time.time() - self._start_time)

        # System metrics
        try:
            import psutil

            process = psutil.Process()
            MEMORY_USAGE.set(process.memory_info().rss)
            CPU_USAGE.set(process.cpu_percent())
        except ImportError:
            pass

        # Orchestrator metrics
        if self.orchestrator:
            state = self.orchestrator.get_state()

            # Worker metrics
            workers = state.get("workers", {})
            active_count = sum(1 for w in workers.values() if w.get("status") == "active")
            pending_count = sum(1 for w in workers.values() if w.get("status") == "pending")
            suspended_count = sum(1 for w in workers.values() if w.get("status") == "suspended")

            WORKERS_TOTAL.labels(status="active").set(active_count)
            WORKERS_TOTAL.labels(status="pending").set(pending_count)
            WORKERS_TOTAL.labels(status="suspended").set(suspended_count)

            # Worker tiers
            tier_counts = {"Probation": 0, "Standard": 0, "Premium": 0}
            for worker in workers.values():
                tier = worker.get("tier", "Probation")
                tier_counts[tier] = tier_counts.get(tier, 0) + 1

            for tier, count in tier_counts.items():
                WORKERS_BY_TIER.labels(tier=tier).set(count)

            # Trust score and bandwidth distributions
            for worker in workers.values():
                trust = worker.get("trust_score", 0)
                bandwidth = worker.get("bandwidth_mbps", 0)
                WORKER_TRUST_SCORE.observe(trust)
                WORKER_BANDWIDTH.observe(bandwidth)

            # Task metrics
            tasks_in_progress = state.get("tasks_in_progress", 0)
            TASKS_IN_PROGRESS.set(tasks_in_progress)

            # Epoch metrics
            EPOCH_NUMBER.set(state.get("current_epoch", 0))
            EPOCH_TASKS.set(state.get("epoch_tasks", 0))
            EPOCH_BYTES.set(state.get("epoch_bytes", 0))

    # =========================================================================
    # Public Metric Update Methods
    # =========================================================================

    def record_request(self, method: str, endpoint: str, status: int, duration: float) -> None:
        """Record HTTP request metrics."""
        REQUEST_COUNT.labels(method=method, endpoint=endpoint, status=status).inc()
        REQUEST_LATENCY.labels(method=method, endpoint=endpoint).observe(duration)

    def record_task(self, task_type: str, status: str, duration: float) -> None:
        """Record task completion metrics."""
        TASKS_TOTAL.labels(type=task_type, status=status).inc()
        TASK_DURATION.labels(type=task_type).observe(duration)

    def record_transfer(
        self, destination_type: str, status: str, bytes_transferred: int, throughput_mbps: float
    ) -> None:
        """Record transfer metrics."""
        TRANSFERS_TOTAL.labels(destination_type=destination_type, status=status).inc()
        TRANSFER_BYTES.labels(direction="egress").inc(bytes_transferred)
        TRANSFER_THROUGHPUT.observe(throughput_mbps)

    def record_rate_limit(self, limit_type: str) -> None:
        """Record rate limit hit."""
        RATE_LIMIT_HITS.labels(type=limit_type).inc()

    def set_blocked_clients(self, count: int) -> None:
        """Update blocked clients count."""
        BLOCKED_CLIENTS.set(count)


# Global metrics collector instance
_metrics_collector: Optional[MetricsCollector] = None


def get_metrics_collector() -> MetricsCollector:
    """Get global metrics collector instance."""
    global _metrics_collector
    if _metrics_collector is None:
        _metrics_collector = MetricsCollector()
    return _metrics_collector


# =============================================================================
# Metrics Middleware
# =============================================================================


class MetricsMiddleware(BaseHTTPMiddleware):
    """
    FastAPI middleware for request metrics collection.

    Automatically records:
    - Request count by method/endpoint/status
    - Request latency
    - Requests in progress
    """

    def __init__(self, app, metrics_collector: Optional[MetricsCollector] = None):
        super().__init__(app)
        self.metrics = metrics_collector or get_metrics_collector()

        # Paths to skip metrics (reduce cardinality)
        self.skip_paths = {"/metrics", "/health/live"}

    def _normalize_path(self, path: str) -> str:
        """Normalize path to reduce metric cardinality."""
        # Replace dynamic segments with placeholders
        parts = path.strip("/").split("/")
        normalized = []
        for part in parts:
            # Detect UUIDs, IDs, hashes
            if len(part) == 36 and part.count("-") == 4:  # UUID
                normalized.append("{id}")
            elif len(part) == 64:  # Hash
                normalized.append("{hash}")
            elif part.isdigit():  # Numeric ID
                normalized.append("{id}")
            else:
                normalized.append(part)
        return "/" + "/".join(normalized) if normalized else "/"

    async def dispatch(self, request: Request, call_next: Callable) -> Response:
        """Process request and collect metrics."""
        # Skip WebSocket requests handled outside BaseHTTPMiddleware
        if request.scope.get("type") == "websocket":
            return await call_next(request)

        path = request.url.path

        # Skip metrics for certain paths
        if path in self.skip_paths:
            return await call_next(request)

        method = request.method
        endpoint = self._normalize_path(path)

        # Track in-progress requests
        REQUEST_IN_PROGRESS.labels(method=method, endpoint=endpoint).inc()

        start_time = time.time()
        try:
            response = await call_next(request)
            status = response.status_code
        except Exception:
            status = 500
            raise
        finally:
            duration = time.time() - start_time
            REQUEST_IN_PROGRESS.labels(method=method, endpoint=endpoint).dec()
            self.metrics.record_request(method, endpoint, status, duration)

        return response


# =============================================================================
# Metrics Endpoint
# =============================================================================


def get_metrics_response() -> tuple[bytes, str]:
    """Generate Prometheus metrics response."""
    if PROMETHEUS_AVAILABLE:
        return generate_latest(), CONTENT_TYPE_LATEST
    else:
        return b"# prometheus_client not installed\n", "text/plain"
