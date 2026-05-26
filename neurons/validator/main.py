"""
BEAM Validator Node Entry Point

Run with: python main.py
"""

import asyncio
import logging
import os
import signal
import sys
from contextlib import asynccontextmanager
from datetime import datetime

import uvicorn
from fastapi import FastAPI, HTTPException


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
            api_key=settings.subnet_core_api_key,
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
    version="0.1.0",
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


@app.get("/analytics/network")
async def get_network_analytics():
    """Get high-level network analytics for public dashboard."""
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    orchestrators = []
    total_bandwidth = 0.0
    healthy_count = 0

    if hasattr(validator, "orchestrator_manager"):
        for orch in validator.orchestrator_manager.get_all_orchestrators():
            score = float(getattr(orch, "last_score", 0.0) or 0.0)
            is_healthy = score > 0.05
            bandwidth = float(getattr(orch, "bandwidth_mbps", 0.0) or 0.0)

            if is_healthy:
                healthy_count += 1

            total_bandwidth += bandwidth

            orchestrators.append(
                {
                    "uid": orch.uid,
                    "hotkey": (
                        orch.hotkey[:8] + "..." + orch.hotkey[-4:] if orch.hotkey else "unknown"
                    ),
                    "is_healthy": is_healthy,
                    "score": round(score, 4),
                    "bandwidth_mbps": round(bandwidth, 2),
                    "worker_count": getattr(orch, "worker_count", 0),
                    "is_subnet_owned": getattr(orch, "is_subnet_owned", False),
                }
            )

    sybil_stats = {"tracked_entities": 0, "suspicious_count": 0}
    if hasattr(validator, "sybil_detector"):
        stats = validator.sybil_detector.get_statistics()
        sybil_stats = {
            "tracked_entities": stats.get("tracked_entities", 0),
            "suspicious_count": stats.get("suspicious_entities", 0),
            "unique_ips": stats.get("unique_ips", 0),
        }

    health_status = "healthy"
    if hasattr(validator, "health_monitor") and validator.health_monitor:
        health_status = validator.health_monitor.get_status().value

    return {
        "network": {
            "validator_hotkey": (
                validator.hotkey[:8] + "..." + validator.hotkey[-4:]
                if validator.hotkey
                else "unknown"
            ),
            "validator_uid": validator.uid,
            "current_block": validator.subtensor.block if validator.subtensor else 0,
            "last_weight_block": validator.last_weight_block,
            "health_status": health_status,
        },
        "orchestrators": {
            "total": len(orchestrators),
            "healthy": healthy_count,
            "unhealthy": len(orchestrators) - healthy_count,
            "total_bandwidth_mbps": round(total_bandwidth, 2),
        },
        "anti_gaming": sybil_stats,
        "orchestrator_list": sorted(
            orchestrators, key=lambda item: item["score"], reverse=True
        ),
    }


@app.get("/analytics/orchestrators")
async def get_orchestrator_analytics():
    """Get detailed orchestrator analytics."""
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    orchestrators = []
    if hasattr(validator, "orchestrator_manager"):
        for orch in validator.orchestrator_manager.get_all_orchestrators():
            orch_data = {
                "uid": orch.uid,
                "hotkey": orch.hotkey,
                "is_subnet_owned": getattr(orch, "is_subnet_owned", False),
                "worker_count": getattr(orch, "worker_count", 0),
            }

            orch_data["score"] = round(float(getattr(orch, "last_score", 0.0) or 0.0), 4)

            if hasattr(validator, "_get_sybil_penalty_multipliers"):
                sybil_mults = validator._get_sybil_penalty_multipliers()
                orch_data["sybil_multiplier"] = round(sybil_mults.get(orch.hotkey, 1.0), 4)

            orchestrators.append(orch_data)

    return {
        "count": len(orchestrators),
        "orchestrators": sorted(
            orchestrators,
            key=lambda item: item.get("score", 0),
            reverse=True,
        ),
    }


@app.get("/analytics/leaderboard")
async def get_leaderboard():
    """Get orchestrator leaderboard sorted by performance."""
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    leaderboard = []
    if hasattr(validator, "orchestrator_manager"):
        for orch in validator.orchestrator_manager.get_all_orchestrators():
            if orch.is_subnet_owned:
                continue

            score = float(getattr(orch, "last_score", 0.0) or 0.0)
            bandwidth = float(getattr(orch, "bandwidth_mbps", 0.0) or 0.0)

            leaderboard.append(
                {
                    "rank": 0,
                    "uid": orch.uid,
                    "hotkey_short": (
                        orch.hotkey[:8] + "..." + orch.hotkey[-4:] if orch.hotkey else "unknown"
                    ),
                    "score": round(score * 100, 2),
                    "bandwidth_mbps": round(bandwidth, 2),
                    "worker_count": getattr(orch, "worker_count", 0),
                }
            )

    leaderboard.sort(key=lambda item: item["score"], reverse=True)
    for index, entry in enumerate(leaderboard):
        entry["rank"] = index + 1

    return {
        "updated_at": datetime.utcnow().isoformat(),
        "leaderboard": leaderboard[:50],
    }


@app.get("/analytics/history")
async def get_weight_history(limit: int = 24):
    """Get weight setting history for trend analysis."""
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    history = validator.weights_history[-limit:] if validator.weights_history else []
    return {
        "count": len(history),
        "last_weight_block": validator.last_weight_block,
        "history": history,
    }


@app.get("/analytics/sybil")
async def get_sybil_analytics():
    """Get Sybil detection analytics."""
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, "sybil_detector"):
        return {"error": "Sybil detector not available"}

    detector = validator.sybil_detector
    stats = detector.get_statistics()
    suspicious_list = []
    for hotkey, result in detector.get_suspicious_entities()[:20]:
        suspicious_list.append(
            {
                "hotkey_short": hotkey[:8] + "..." + hotkey[-4:] if hotkey else "unknown",
                "violations": [violation.value for violation in result.violations],
                "confidence": round(result.confidence, 2),
                "penalty_multiplier": round(result.penalty_multiplier, 2),
            }
        )

    return {
        "stats": stats,
        "suspicious_entities": suspicious_list,
    }


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

        log_viewer_url = os.environ.get("LOG_VIEWER_URL", "https://beamcore.b1m.ai/logs/")

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
