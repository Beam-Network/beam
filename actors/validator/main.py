"""
BEAM Validator Node Entry Point

Run with: python main.py

Mainnet settings:
- BEAM_VALIDATOR_CORE_SERVER_URL=https://beamcore.b1m.ai
- SUBTENSOR_NETWORK=finney
- NETUID=105

The validator fetches BeamCore epoch summaries, sets the returned weights on
Bittensor, and posts a weight proof back to BeamCore.
"""

import asyncio
import logging
import os
import signal
import sys
from contextlib import asynccontextmanager

import uvicorn
from fastapi import FastAPI


def _fetch_uid_config_sync():
    """
    Fetch UID ranges from BeamCore synchronously before beam-dependent imports.
    """
    import httpx

    core_url = os.getenv("BEAM_VALIDATOR_CORE_SERVER_URL", "https://beamcore.b1m.ai")

    try:
        response = httpx.get(f"{core_url}/config/uid-ranges", timeout=5.0)
        response.raise_for_status()
        data = response.json()

        os.environ["PUBLIC_ORCHESTRATOR_UID_START"] = str(data["public_orchestrator_uid_start"])
        os.environ["PUBLIC_ORCHESTRATOR_UID_END"] = str(data["public_orchestrator_uid_end"])
        os.environ["MAX_ORCHESTRATORS"] = str(data["max_orchestrators"])

        print(
            f"[Startup] Fetched UID config from {core_url}: "
            f"public={data['public_orchestrator_uid_start']}-{data['public_orchestrator_uid_end']}, "
            f"max_orchestrators={data['max_orchestrators']}"
        )
    except Exception as exc:
        print(f"[Startup] Warning: Could not fetch UID config from {core_url}: {exc}")
        print("[Startup] Using default UID ranges from environment or beam defaults")


_fetch_uid_config_sync()

from clients import close_subnet_core_client, init_subnet_core_client
from core.config import get_settings
from core.validator import Validator

LOG_DIR = os.environ.get("LOG_DIR", "/tmp/beam_validator_logs")
os.makedirs(LOG_DIR, exist_ok=True)

log_format = "%(asctime)s | %(levelname)s | %(name)s | %(message)s"
log_datefmt = "%Y-%m-%d %H:%M:%S"

logging.basicConfig(
    level=logging.INFO,
    format=log_format,
    datefmt=log_datefmt,
)

file_handler = logging.FileHandler(f"{LOG_DIR}/validator.log")
file_handler.setFormatter(logging.Formatter(log_format, datefmt=log_datefmt))
logging.getLogger().addHandler(file_handler)

logger = logging.getLogger(__name__)

validator: Validator = None


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Application lifespan manager."""
    global validator

    settings = get_settings()
    logging.getLogger().setLevel(settings.log_level)

    validator = Validator(settings)
    await validator.initialize()

    signed_auth = "disabled"
    if settings.core_server_url and validator.hotkey:
        subnet_core_client = init_subnet_core_client(
            base_url=settings.core_server_url,
            validator_hotkey=validator.hotkey,
            wallet=validator.wallet,
        )
        validator.subnet_core_client = subnet_core_client
        signed_auth = "enabled" if validator.wallet else "disabled"

    network = validator.subtensor.network if validator.subtensor else "unknown"
    fiber_status = "available" if validator.fiber_chain else "unavailable"
    _rows = [
        ("Hotkey",       validator.hotkey or "—"),
        ("UID",          str(validator.uid) if validator.uid is not None else "—"),
        ("Subnet",       f"{settings.netuid} ({network})"),
        ("BeamCore",     settings.core_server_url or "—"),
        ("Signed Auth",  signed_auth),
        ("Fiber",        fiber_status),
        ("Local Mode",   str(settings.local_mode)),
        ("Port",         str(settings.port)),
        ("External URL", settings.external_url or "—"),
    ]
    _kw, _vw = 16, max(len(v) for _, v in _rows)
    _top   = f"┌{'─' * (_kw + _vw + 5)}┐"
    _title = f"│{'BEAM Validator Ready':^{_kw + _vw + 5}}│"
    _sep   = f"├{'─' * (_kw + 2)}┬{'─' * (_vw + 2)}┤"
    _body  = "\n".join(f"│ {k:<{_kw}} │ {v:<{_vw}} │" for k, v in _rows)
    _bot   = f"└{'─' * (_kw + 2)}┴{'─' * (_vw + 2)}┘"
    await validator.start()

    print("\n".join([_top, _title, _sep, _body, _bot]), flush=True)

    yield

    await validator.stop()
    await close_subnet_core_client()
    logger.info("BEAM Validator node stopped")


app = FastAPI(
    title="BEAM Validator",
    description="BEAM Validator Node API",
    version="0.2.0",
    lifespan=lifespan,
)


@app.get("/health")
async def health():
    """Health check endpoint."""
    settings = get_settings()
    if validator and validator.health_monitor:
        report = await validator.health_monitor.run_health_checks()
        return {
            "status": report.status.value,
            "node_type": "validator",
            "external_url": settings.external_url,
            "checks": [check.to_dict() for check in report.checks],
            "consecutive_failures": report.consecutive_failures,
            "uptime_seconds": report.uptime_seconds,
        }
    return {"status": "healthy", "node_type": "validator", "external_url": settings.external_url}


@app.get("/health/detailed")
async def health_detailed():
    """Detailed health check with all component statuses."""
    if validator and validator.health_monitor:
        report = await validator.health_monitor.run_health_checks()
        return report.to_dict()
    return {"status": "unknown", "message": "Health monitor not available"}


@app.get("/state")
async def get_state():
    """Get validator state."""
    if validator:
        return validator.get_validator_state()
    return {"error": "Validator not initialized"}


@app.get("/scores")
async def get_scores():
    """Get connection scores."""
    if validator:
        return {"scores": validator.get_connection_scores()}
    return {"error": "Validator not initialized"}


@app.get("/weights")
async def get_weights():
    """Get weight history."""
    if validator:
        return {
            "last_weight_block": validator.last_weight_block,
            "history": validator.weights_history[-10:],
        }
    return {"error": "Validator not initialized"}


def main():
    """Main entry point."""
    settings = get_settings()

    def signal_handler(sig, frame):
        logger.info("Shutdown signal received")
        sys.exit(0)

    signal.signal(signal.SIGINT, signal_handler)
    signal.signal(signal.SIGTERM, signal_handler)

    if os.environ.get("OPEN_LOG_VIEWER", "").lower() in ("true", "1", "yes"):
        import threading
        import time
        import webbrowser

        log_viewer_url = os.environ.get("LOG_VIEWER_URL", "http://localhost:8080/logs/")

        def open_logs():
            time.sleep(1.5)
            webbrowser.open(log_viewer_url)

        threading.Thread(target=open_logs, daemon=True).start()

    uvicorn.run(
        app,
        host="0.0.0.0",
        port=settings.port,
        log_level=settings.log_level.lower(),
    )


if __name__ == "__main__":
    main()
