"""Read-only public views over validator-owned state."""

from typing import Dict, List


def get_validator_state(validator) -> dict:
    beamcore_total_workers = sum(validator._beamcore_worker_counts.values())
    worker_stats = validator.worker_registry.get_stats()
    reassignment_stats = validator.reassignment_manager.get_stats()
    return {
        "uid": validator.uid,
        "hotkey": validator.hotkey,
        "is_registered": validator.is_registered,
        "connections_tracked": len(validator.connections),
        "pending_tasks": len(validator.pending_tasks),
        "verified_proofs": len([r for r in validator.task_results.values() if r.valid]),
        "last_weight_block": validator.last_weight_block,
        "current_block": validator.subtensor.block if validator.subtensor else 0,
        "orchestrators": {
            "total_orchestrators": len(validator.orchestrators),
            "total_workers": beamcore_total_workers,
            "worker_counts_by_uid": dict(validator._beamcore_worker_counts),
        },
        "workers": worker_stats,
        "reassignments": reassignment_stats,
        "total_redirected_to_one_tao": validator.total_redirected_to_one,
    }


def get_connection_scores(validator) -> Dict[int, dict]:
    return {
        uid: {
            "score": round(validator.connection_scores.get(uid, 0), 4),
            "bandwidth_mbps": round(validator.connection_bandwidth.get(uid, 0), 2),
            "hotkey": connection["hotkey"],
        }
        for uid, connection in validator.connections.items()
    }


def get_orchestrator_scores(validator) -> Dict[str, dict]:
    result = {}
    for hotkey, info in validator.orchestrators.items():
        uid = validator._get_uid_for_hotkey(hotkey)
        result[hotkey] = {
            "uid": uid,
            "score": round(validator.orchestrator_scores.get(hotkey, 0), 4),
            "url": info.url,
            "is_healthy": info.is_healthy,
            "is_subnet_owned": info.is_subnet_owned,
            "last_seen": info.last_seen.isoformat(),
            "registered": uid is not None,
        }
    return result


def get_spot_check_results(validator) -> Dict[str, dict]:
    results = {}
    for hotkey, result in validator.spot_check_results.items():
        results[hotkey[:16]] = {
            "proofs_requested": result.proofs_requested,
            "proofs_received": result.proofs_received,
            "proofs_valid": result.proofs_valid,
            "proofs_invalid": result.proofs_invalid,
            "verification_rate": round(result.verification_rate, 4),
            "fraud_detected": result.fraud_detected,
            "fraud_severity": round(result.fraud_severity, 4),
            "fraud_multiplier": round(validator._calculate_fraud_multiplier(hotkey), 4),
            "timestamp": result.timestamp.isoformat(),
        }
    return results


def get_weights_history(validator, limit: int = 10) -> List[dict]:
    return list(reversed(validator.weights_history[-limit:]))
