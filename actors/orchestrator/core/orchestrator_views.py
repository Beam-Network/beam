"""Read-only state views and epoch-summary aggregation."""

from datetime import datetime
from typing import List, Optional

from .models import EpochSummary, WorkerStatus


def _build_epoch_summary(orchestrator) -> EpochSummary:
    workers = list(orchestrator.workers.values())
    active_workers = [worker for worker in workers if worker.bytes_relayed_epoch > 0]
    total_bytes = sum(worker.bytes_relayed_epoch for worker in workers)
    total_tasks = sum(worker.successful_tasks for worker in workers)
    failed_tasks = sum(worker.failed_tasks for worker in workers)
    total_completed = total_tasks + failed_tasks

    return EpochSummary(
        epoch=orchestrator.current_epoch,
        start_time=orchestrator.epoch_start_time,
        end_time=datetime.utcnow(),
        total_tasks=total_completed,
        successful_tasks=total_tasks,
        failed_tasks=failed_tasks,
        total_bytes_relayed=total_bytes,
        active_workers=len(active_workers),
        worker_contributions={
            worker.worker_id: worker.bytes_relayed_epoch
            for worker in active_workers
        },
        success_rate=(total_tasks / total_completed) if total_completed else 0.0,
    )

def get_state(orchestrator) -> dict:
    """Get current Orchestrator state."""
    active_workers = [w for w in orchestrator.workers.values() if w.is_available]

    return {
        "hotkey": orchestrator.hotkey,
        "beamcore_upstream_degraded": (
            orchestrator.subnet_core_client.is_beamcore_upstream_degraded()
            if getattr(orchestrator, "subnet_core_client", None)
            else None
        ),
        "current_epoch": orchestrator.current_epoch,
        "epoch_start": orchestrator.epoch_start_time.isoformat(),
        "total_workers": len(orchestrator.workers),
        "active_workers": len(active_workers),
        "workers_by_status": {
            status.value: len([w for w in orchestrator.workers.values() if w.status == status])
            for status in WorkerStatus
        },
        "active_tasks": len(orchestrator.active_tasks),
        "total_bytes_relayed": orchestrator.total_bytes_relayed,
        "total_tasks_completed": orchestrator.total_tasks_completed,
    }

def get_worker_stats(orchestrator) -> List[dict]:
    """Get statistics for all workers."""
    return [
        {
            "worker_id": w.worker_id,
            "region": w.region,
            "status": w.status.value,
            "trust_score": round(w.trust_score, 4),
            "bandwidth_mbps": round(w.bandwidth_ema, 2),
            "success_rate": round(w.success_rate, 4),
            "total_tasks": w.total_tasks,
            "bytes_relayed": w.bytes_relayed_total,
            "load_factor": round(w.load_factor, 2),
        }
        for w in orchestrator.workers.values()
    ]

def get_epoch_stats(orchestrator, epoch: Optional[int] = None) -> Optional[dict]:
    """Get statistics for an epoch."""
    if epoch is None:
        epoch = orchestrator.current_epoch

    summary = orchestrator.epoch_summaries.get(epoch)
    if not summary:
        if epoch == orchestrator.current_epoch:
            summary = orchestrator._build_epoch_summary()
        else:
            return None

    return {
        "epoch": summary.epoch,
        "start_time": summary.start_time.isoformat(),
        "end_time": summary.end_time.isoformat(),
        "total_tasks": summary.total_tasks,
        "total_bytes_relayed": summary.total_bytes_relayed,
        "active_workers": summary.active_workers,
        "avg_bandwidth_mbps": round(summary.avg_bandwidth_mbps, 2),
        "avg_latency_ms": round(summary.avg_latency_ms, 2),
        "success_rate": round(summary.success_rate, 4),
    }
