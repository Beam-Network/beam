"""
Health and Status API Routes

Endpoints for monitoring Orchestrator health and statistics.
"""

import logging
from typing import Optional

from fastapi import APIRouter, Depends

from core.orchestrator import get_orchestrator, Orchestrator

logger = logging.getLogger(__name__)

router = APIRouter(tags=["health"])


# =============================================================================
# Health Check
# =============================================================================

@router.get("/health")
async def health_check():
    """Basic health check endpoint."""
    return {"status": "healthy", "service": "beam-orchestrator"}


@router.get("/ready")
async def readiness_check(
    orchestrator: Orchestrator = Depends(lambda: get_orchestrator()),
):
    """
    Readiness check - verifies Orchestrator is ready to accept requests.

    Checks:
    - Bittensor connection
    - Worker availability
    - Background tasks running
    """
    ready = True
    checks = {}

    # Check wallet
    checks["wallet"] = orchestrator.wallet is not None

    # Check subtensor connection
    checks["subtensor"] = orchestrator.subtensor is not None

    # Check metagraph
    checks["metagraph"] = orchestrator.metagraph is not None

    # Check for available workers
    active_workers = len([
        w for w in orchestrator.workers.values()
        if w.is_available
    ])
    checks["workers_available"] = active_workers > 0

    # Check background tasks
    checks["background_tasks"] = len(orchestrator._background_tasks) > 0

    # Overall ready status
    ready = all(checks.values())

    return {
        "ready": ready,
        "checks": checks,
        "active_workers": active_workers,
    }


# =============================================================================
# Status & Metrics
# =============================================================================

@router.get("/status")
async def get_status(
    orchestrator: Orchestrator = Depends(lambda: get_orchestrator()),
):
    """Get detailed Orchestrator status."""
    return orchestrator.get_state()


@router.get("/metrics")
async def get_metrics(
    orchestrator: Orchestrator = Depends(lambda: get_orchestrator()),
):
    """
    Get Prometheus-compatible metrics.

    Returns metrics in a format suitable for monitoring systems.
    """
    state = orchestrator.get_state()

    # Build metrics
    metrics = []

    # Worker metrics
    metrics.append(f"beam_workers_total {state['total_workers']}")
    metrics.append(f"beam_workers_active {state['active_workers']}")

    for status, count in state["workers_by_status"].items():
        metrics.append(f'beam_workers_by_status{{status="{status}"}} {count}')

    # Task metrics
    metrics.append(f"beam_tasks_active {state['active_tasks']}")
    metrics.append(f"beam_tasks_completed_total {state['total_tasks_completed']}")

    # Byte metrics
    metrics.append(f"beam_bytes_relayed_total {state['total_bytes_relayed']}")

    # Proof metrics
    metrics.append(f"beam_proofs_pending {state['pending_proofs']}")

    # Epoch metrics
    metrics.append(f"beam_current_epoch {state['current_epoch']}")

    # Validator metrics
    metrics.append(f"beam_validators_known {state['validators_known']}")

    return "\n".join(metrics)


@router.get("/epoch")
async def get_epoch_info(
    epoch: Optional[int] = None,
    orchestrator: Orchestrator = Depends(lambda: get_orchestrator()),
):
    """Get information about a specific epoch or current epoch."""
    return orchestrator.get_epoch_stats(epoch)


@router.get("/epoch/history")
async def get_epoch_history(
    limit: int = 10,
    orchestrator: Orchestrator = Depends(lambda: get_orchestrator()),
):
    """Get history of recent epochs."""
    epochs = sorted(orchestrator.epoch_summaries.keys(), reverse=True)[:limit]

    return {
        "current_epoch": orchestrator.current_epoch,
        "history": [
            orchestrator.get_epoch_stats(e)
            for e in epochs
        ],
    }
