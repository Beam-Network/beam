"""
BEAM Orchestrator Entry Point

Run with: python -m neurons.orchestrator.main

The Orchestrator coordinates bandwidth tasks with BeamCore:
- Registers with BeamCore HTTP on startup
- Keeps a live NATS control session to BeamCore for assignments and control updates
- Relies on Core NATS for the assignment and task-result hot path
- Manages the orchestrator's advertised worker pool and forwards worker outcomes upstream to BeamCore

Architecture:
BeamCore <-> Core NATS <-> Orchestrator <-> Worker Gateway <-> Workers
Transfer execution happens on workers that dial the orchestrator-owned worker gateways advertised to BeamCore.
Orchestrators coordinate through BeamCore and their advertised worker gateway.
"""

import logging
import os
import socket
import time
from contextlib import asynccontextmanager

import uvicorn
from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from core.config import get_settings
from core.orchestrator import Orchestrator, get_orchestrator
from middleware.metrics import MetricsMiddleware, get_metrics_collector, get_metrics_response
from middleware.rate_limiting import RateLimitMiddleware, get_rate_limiter
from routes import health, orchestrators, workers

# NATS control registration, keepalive, and transfer flow are owned by
# SubnetCoreClient. main.py only wires lifespan + FastAPI routes.


# Configure logging - both console and file
LOG_DIR = os.environ.get("LOG_DIR", "/tmp/beam_logs")
os.makedirs(LOG_DIR, exist_ok=True)

log_format = "%(asctime)s | %(levelname)s | %(name)s | %(message)s"
log_datefmt = "%Y-%m-%d %H:%M:%S"

logging.basicConfig(
    level=logging.INFO,
    format=log_format,
    datefmt=log_datefmt,
)

# Add file handler for log viewer
file_handler = logging.FileHandler(f"{LOG_DIR}/orchestrator.log")
file_handler.setFormatter(logging.Formatter(log_format, datefmt=log_datefmt))
logging.getLogger().addHandler(file_handler)

logger = logging.getLogger(__name__)

# Global instances
orchestrator: Orchestrator = None


def _get_local_ip() -> str:
    """Best-effort local outbound IP (registration URL when EXTERNAL_IP is unset)."""
    try:
        # Create a socket to determine the outbound IP
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.connect(("8.8.8.8", 80))
        ip = s.getsockname()[0]
        s.close()
        return ip
    except Exception:
        return "127.0.0.1"


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Application lifespan manager."""
    global orchestrator

    settings = get_settings()

    # Configure logging level
    logging.getLogger().setLevel(settings.log_level)

    # Initialize rate limiter
    rate_limiter = get_rate_limiter()
    await rate_limiter.start_cleanup()

    # Worker and transfer coordination are handled by BeamCore

    # Initialize metrics collector
    metrics_collector = get_metrics_collector()

    # Initialize orchestrator
    orchestrator = get_orchestrator()
    await orchestrator.initialize()

    # Client authentication is handled by BeamCore

    # Link metrics collector to orchestrator
    metrics_collector.set_orchestrator(orchestrator)
    await metrics_collector.start()

    # Start orchestrator background tasks
    await orchestrator.start()

    logger.info("=" * 60)
    logger.info("BEAM Orchestrator started")
    logger.info("=" * 60)
    logger.info(f"Hotkey: {orchestrator.hotkey}")
    logger.info(f"Network: {settings.subtensor_network}")
    logger.info(f"Subnet: {settings.netuid}")
    logger.info(f"API: http://{settings.api_host}:{settings.api_port}")
    logger.info("=" * 60)

    # NATS control connection (registration + keepalive + transfer flow) is owned by
    # SubnetCoreClient. It auto-registers via NATS using the config set in
    # _init_subnet_core_client and obtains an API key via /auth/challenge + /auth/verify.
    if orchestrator.subnet_core_client:
        api_key = orchestrator.subnet_core_client._api_key
        if api_key:
            logger.info("SubnetCoreClient API key cached in memory (%s...)", api_key[:20])
        else:
            logger.info(
                "SubnetCoreClient API key will be fetched during the first NATS control connection; "
                "set BEAMCORE_API_KEY to use a pre-issued key "
            )
    else:
        logger.warning("SubnetCoreClient unavailable")
    logger.info("NATS control connection handled by SubnetCoreClient")

    # Record operator readiness; BeamCore routing is enabled only while local workers are connected.
    if settings.ready and orchestrator.subnet_core_client:
        orchestrator.subnet_core_client.prime_ready_state(True)
        try:
            applied = await orchestrator.subnet_core_client.sync_ready_if_eligible()
            if applied:
                logger.info(
                    "Signalled ready=True through NATS control; orchestrator will receive transfers"
                )
            else:
                logger.info(
                    "Ready intent recorded; BeamCore routing will enable when local workers connect"
                )
        except Exception as e:
            logger.warning(f"Failed to set ready=True through NATS control: {e}")
    else:
        logger.info(
            "ready=False; set READY=true when local workers are ready for transfers"
        )

    yield

    # Cleanup
    logger.info("Shutting down BEAM Orchestrator...")

    # Signal not-ready before stopping so BeamCore stops routing traffic immediately
    if orchestrator.subnet_core_client:
        try:
            applied = await orchestrator.subnet_core_client.set_ready(False)
            if applied:
                logger.info(
                    "Signalled ready=False through NATS control; routing disabled"
                )
            else:
                logger.info("Queued ready=False for NATS control shutdown sync")
        except Exception as e:
            logger.warning(f"Failed to set ready=False through NATS control during shutdown: {e}")

    await orchestrator.stop()
    await metrics_collector.stop()
    await rate_limiter.stop_cleanup()

    logger.info("BEAM Orchestrator stopped")


# Create FastAPI app
app = FastAPI(
    title="BEAM Orchestrator",
    description="""
BEAM Orchestrator - Decentralized bandwidth mining coordinator.

The Orchestrator connects to BeamCore and:
- Maintains a live NATS control session
- Advertises an orchestrator-owned worker gateway
- Receives task offer batches from BeamCore
- Routes task offers to connected local workers
- Relays worker task results upstream

Workers register with BeamCore for identity and API keys, then connect to
the advertised worker gateway for task delivery.

## Endpoints

### Health
Monitor the Orchestrator's health and view metrics.

### Orchestrators
Registration and readiness endpoints for BeamCore communication.
    """,
    version="0.1.0",
    lifespan=lifespan,
)

# Add middleware (order matters - first added = last to process request)
app.add_middleware(MetricsMiddleware, metrics_collector=get_metrics_collector())
app.add_middleware(RateLimitMiddleware, rate_limiter=get_rate_limiter())

# Add CORS middleware if configured
_cors_settings = get_settings()
_cors_origins = _cors_settings.get_cors_origins()
if _cors_origins:
    app.add_middleware(
        CORSMiddleware,
        allow_origins=_cors_origins,
        allow_credentials=_cors_settings.cors_allow_credentials,
        allow_methods=_cors_settings.get_cors_methods(),
        allow_headers=_cors_settings.get_cors_headers(),
    )
    logger.info(f"CORS enabled for origins: {_cors_origins}")

# Mount route modules
app.include_router(health.router)
app.include_router(orchestrators.router)
app.include_router(workers.router)


# =============================================================================
# Additional API Routes
# =============================================================================


@app.get("/")
async def root():
    """Root endpoint with API info."""
    return {
        "service": "BEAM Orchestrator",
        "version": "0.1.0",
        "description": "Central coordinator for decentralized bandwidth mining",
        "docs": "/docs",
        "health": "/health",
    }


@app.get("/state")
async def get_state():
    """Get full Orchestrator state."""
    if orchestrator:
        return orchestrator.get_state()
    return {"error": "Orchestrator not initialized"}


@app.get("/workers/stats")
async def get_worker_stats():
    """Get detailed worker statistics."""
    if orchestrator:
        return orchestrator.get_worker_stats()
    return {"error": "Orchestrator not initialized"}


@app.get("/metrics")
async def metrics():
    """Prometheus metrics endpoint."""
    from fastapi.responses import Response

    content, content_type = get_metrics_response()
    return Response(content=content, media_type=content_type)


@app.get("/metrics/json")
async def metrics_json():
    """JSON metrics endpoint for non-Prometheus consumers."""
    metrics_collector = get_metrics_collector()
    rate_limiter = get_rate_limiter()

    return {
        "uptime_seconds": time.time() - metrics_collector._start_time,
        "orchestrator": orchestrator.get_state() if orchestrator else {},
        "rate_limiter": rate_limiter.get_stats(),
    }


# =============================================================================
# Main
# =============================================================================


def main():
    """Main entry point."""
    settings = get_settings()

    # Print banner
    print("""
====================================================
BEAM ORCHESTRATOR
Decentralized Bandwidth Mining Coordinator
====================================================
    """)
    # Auto-open log viewer in browser (disabled by default, set OPEN_LOG_VIEWER=true to enable)
    if os.environ.get("OPEN_LOG_VIEWER", "").lower() in ("true", "1", "yes"):
        import threading
        import webbrowser

        log_viewer_url = os.environ.get("LOG_VIEWER_URL", "http://localhost:8080/logs/")

        def open_logs():
            time.sleep(1.5)  # Wait for server to start
            webbrowser.open(log_viewer_url)

        threading.Thread(target=open_logs, daemon=True).start()

    # Run server
    uvicorn.run(
        app,
        host=settings.api_host,
        port=settings.api_port,
        log_level=settings.log_level.lower(),
        ws_ping_interval=25.0,
        ws_ping_timeout=float(os.environ.get("WORKER_WS_PING_TIMEOUT", "120")),
    )


if __name__ == "__main__":
    main()
