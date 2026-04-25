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

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from typing import Dict, Optional
import uvicorn

# =============================================================================
# IMPORTANT: Fetch UID config from SubnetCore BEFORE importing beam modules
# The beam library reads UID ranges from environment variables at import time
# =============================================================================


def _fetch_uid_config_sync():
    """
    Fetch UID ranges from BeamCore synchronously.
    Must be called before importing beam modules.
    """
    import httpx

    core_url = os.getenv("BEAM_VALIDATOR_SUBNET_CORE_URL") or os.getenv("BEAM_SUBNET_CORE_URL", "http://localhost:8080")

    try:
        response = httpx.get(f"{core_url}/config/uid-ranges", timeout=5.0)
        response.raise_for_status()
        data = response.json()

        # Set environment variables before beam library is imported
        os.environ["SUBNET_ORCHESTRATOR_UID"] = str(data["subnet_orchestrator_uid"])
        os.environ["PUBLIC_ORCHESTRATOR_UID_START"] = str(data["public_orchestrator_uid_start"])
        os.environ["PUBLIC_ORCHESTRATOR_UID_END"] = str(data["public_orchestrator_uid_end"])
        os.environ["RESERVED_ORCHESTRATOR_UID_START"] = str(data["reserved_orchestrator_uid_start"])
        os.environ["RESERVED_ORCHESTRATOR_UID_END"] = str(data["reserved_orchestrator_uid_end"])
        os.environ["MAX_ORCHESTRATORS"] = str(data["max_orchestrators"])

        print(f"[Startup] Fetched UID config from {core_url}: "
              f"public={data['public_orchestrator_uid_start']}-{data['public_orchestrator_uid_end']}, "
              f"reserved={data['reserved_orchestrator_uid_start']}-{data['reserved_orchestrator_uid_end']}")


    except Exception as e:
        print(f"[Startup] Warning: Could not fetch UID config from {core_url}: {e}")
        print("[Startup] Using default UID ranges from environment or beam defaults")


# Fetch config before importing beam-dependent modules
_fetch_uid_config_sync()

# Now safe to import beam-dependent modules
from core.validator import Validator
from core.config import get_settings
from clients import init_subnet_core_client, close_subnet_core_client, get_subnet_core_client

# Configure logging - both console and file
LOG_DIR = os.environ.get("LOG_DIR", "/tmp/terapipe_logs")
os.makedirs(LOG_DIR, exist_ok=True)

log_format = "%(asctime)s | %(levelname)s | %(name)s | %(message)s"
log_datefmt = "%Y-%m-%d %H:%M:%S"

logging.basicConfig(
    level=logging.INFO,
    format=log_format,
    datefmt=log_datefmt,
)

# Add file handler for log viewer
file_handler = logging.FileHandler(f"{LOG_DIR}/validator.log")
file_handler.setFormatter(logging.Formatter(log_format, datefmt=log_datefmt))
logging.getLogger().addHandler(file_handler)

logger = logging.getLogger(__name__)

# Global validator instance
validator: Validator = None


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Application lifespan manager"""
    global validator

    settings = get_settings()

    # Configure logging level
    logging.getLogger().setLevel(settings.log_level)

    # Initialize SubnetCore API client (replaces direct database access)
    # The validator now submits scores via API to the private BeamCore service
    if settings.subnet_core_url:
        logger.info(f"Initializing SubnetCore API client: {settings.subnet_core_url}")
        # We need the validator hotkey for authentication - initialize after validator
        # For now, just log the URL; actual init happens after validator.initialize()

    # Use unified Validator that supports both local mode (HTTP) and mainnet/testnet (dendrite)
    logger.info(f"Initializing Validator (local_mode={settings.local_mode})")
    validator = Validator(settings)

    await validator.initialize()

    # Initialize SubnetCore API client now that we have the validator hotkey
    if settings.subnet_core_url and validator.hotkey:
        subnet_core_client = init_subnet_core_client(
            base_url=settings.subnet_core_url,
            validator_hotkey=validator.hotkey,
            wallet=validator.wallet,  # Pass wallet for signed auth
        )
        # Attach to validator for use in score submission
        validator.subnet_core_client = subnet_core_client
        signed_auth = "enabled" if validator.wallet else "disabled"
        logger.info(f"SubnetCore API client initialized for hotkey {validator.hotkey[:16]}... (signed_auth={signed_auth})")

    # Start validator in background
    asyncio.create_task(validator.start())

    logger.info("BEAM Validator node started")
    if settings.external_url:
        logger.info(f"External URL: {settings.external_url}")

    yield

    # Cleanup
    await validator.stop()
    await close_subnet_core_client()
    logger.info("BEAM Validator node stopped")


# Create FastAPI app
app = FastAPI(
    title="BEAM Validator",
    description="BEAM Validator Node API",
    version="0.1.0",
    lifespan=lifespan,
)


# =============================================================================
# API Routes
# =============================================================================

@app.get("/health")
async def health():
    """Health check endpoint"""
    settings = get_settings()
    if validator and validator.health_monitor:
        report = await validator.health_monitor.run_health_checks()
        return {
            "status": report.status.value,
            "node_type": "validator",
            "external_url": settings.external_url,
            "checks": [c.to_dict() for c in report.checks],
            "consecutive_failures": report.consecutive_failures,
            "uptime_seconds": report.uptime_seconds,
        }
    return {"status": "healthy", "node_type": "validator", "external_url": settings.external_url}


@app.get("/health/detailed")
async def health_detailed():
    """Detailed health check with all component statuses"""
    if validator and validator.health_monitor:
        report = await validator.health_monitor.run_health_checks()
        return report.to_dict()
    return {"status": "unknown", "message": "Health monitor not available"}


@app.get("/state")
async def get_state():
    """Get validator state"""
    if validator:
        return validator.get_validator_state()
    return {"error": "Validator not initialized"}


@app.get("/scores")
async def get_scores():
    """Get connection scores"""
    if validator:
        return {"scores": validator.get_connection_scores()}
    return {"error": "Validator not initialized"}


@app.get("/weights")
async def get_weights():
    """Get weight history"""
    if validator:
        return {
            "last_weight_block": validator.last_weight_block,
            "history": validator.weights_history[-10:],  # Last 10
        }
    return {"error": "Validator not initialized"}


# =============================================================================
# PoB Submission Models
# =============================================================================

class PoBSubmission(BaseModel):
    """Proof-of-Bandwidth submission from Connection or Orchestrator."""
    task_id: str
    miner_hotkey: str
    worker_id: str
    start_time_us: int
    end_time_us: int
    bytes_relayed: int = 10_485_760  # 10 MB default
    bandwidth_mbps: float
    canary_proof: str
    merkle_path: list = []
    path_index: int = 0
    signature: str


class PoBSubmissionResponse(BaseModel):
    """Response for PoB submission."""
    success: bool
    task_id: str
    valid: bool = False
    calculated_bandwidth: Optional[float] = None
    error: Optional[str] = None


# =============================================================================
# PoB Submission Endpoint
# =============================================================================

@app.post("/pob/submit", response_model=PoBSubmissionResponse)
async def submit_pob(submission: PoBSubmission):
    """
    Receive and verify Proof-of-Bandwidth submission.

    Called by Connection nodes or Orchestrators after workers complete relays.
    The validator verifies the PoB cryptographically and records it for scoring.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    try:
        # Import required modules
        from core._beam_stubs import ProofOfBandwidth, sha256

        # Convert submission to ProofOfBandwidth object
        pob = ProofOfBandwidth(
            task_id=bytes.fromhex(submission.task_id) if len(submission.task_id) == 64
                    else sha256(submission.task_id.encode()),
            miner_id=submission.miner_hotkey.encode(),
            worker_id=submission.worker_id,
            start_time=submission.start_time_us,
            end_time=submission.end_time_us,
            bytes_relayed=submission.bytes_relayed,
            bandwidth_mbps=submission.bandwidth_mbps,
            canary_proof=bytes.fromhex(submission.canary_proof),
            merkle_path=[bytes.fromhex(p) for p in submission.merkle_path] if submission.merkle_path else [],
            path_index=submission.path_index,
            signature=bytes.fromhex(submission.signature) if submission.signature else b"",
        )

        # Get the corresponding task if it exists
        task = validator.pending_tasks.get(submission.task_id)

        # Verify the PoB
        result = validator.verify_pob(pob, task) if task else None

        if result and result.valid:
            # Store the verified result
            validator.task_results[submission.task_id] = result

            logger.info(
                f"PoB verified: task={submission.task_id[:16]}..., "
                f"bandwidth={result.calculated_bandwidth:.2f} Mbps"
            )

            return PoBSubmissionResponse(
                success=True,
                task_id=submission.task_id,
                valid=True,
                calculated_bandwidth=result.calculated_bandwidth,
            )
        elif result:
            logger.warning(
                f"PoB verification failed: task={submission.task_id[:16]}..., "
                f"error={result.error}"
            )
            return PoBSubmissionResponse(
                success=True,
                task_id=submission.task_id,
                valid=False,
                error=result.error,
            )
        else:
            # Task not found - still store for batch verification later
            # This handles the case where PoB arrives before task registration
            if not hasattr(validator, "pending_pob_submissions"):
                validator.pending_pob_submissions = {}

            validator.pending_pob_submissions[submission.task_id] = {
                "pob": pob,
                "submission": submission.model_dump(),
                "received_at": asyncio.get_event_loop().time(),
            }

            logger.info(
                f"PoB queued for verification: task={submission.task_id[:16]}... "
                f"(task not yet registered)"
            )

            return PoBSubmissionResponse(
                success=True,
                task_id=submission.task_id,
                valid=False,
                error="Task not found - queued for later verification",
            )

    except Exception as e:
        logger.error(f"Error processing PoB submission: {e}", exc_info=True)
        return PoBSubmissionResponse(
            success=False,
            task_id=submission.task_id,
            error=str(e),
        )


@app.get("/pob/results")
async def get_pob_results():
    """Get all verified PoB results."""
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    results = {}
    for task_id, result in validator.task_results.items():
        results[task_id] = {
            "valid": result.valid,
            "calculated_bandwidth": result.calculated_bandwidth,
            "latency_ms": result.latency_ms,
            "signature_valid": result.signature_valid,
            "timing_valid": result.timing_valid,
            "bandwidth_valid": result.bandwidth_valid,
            "canary_valid": result.canary_valid,
            "merkle_valid": result.merkle_valid,
            "geo_valid": result.geo_valid,
            "error": result.error,
        }

    return {"results": results, "count": len(results)}


@app.get("/pob/pending")
async def get_pending_pob():
    """Get pending PoB submissions awaiting verification."""
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, "pending_pob_submissions"):
        return {"pending": {}, "count": 0}

    pending = {
        task_id: {
            "worker_id": info["submission"]["worker_id"],
            "bandwidth_mbps": info["submission"]["bandwidth_mbps"],
            "received_at": info["received_at"],
        }
        for task_id, info in validator.pending_pob_submissions.items()
    }

    return {"pending": pending, "count": len(pending)}


# =============================================================================
# Orchestrator Report Models
# =============================================================================

class OrchestratorReport(BaseModel):
    """Report from Orchestrator containing epoch work summary."""
    orchestrator_hotkey: str
    epoch: int
    total_tasks: int
    successful_tasks: int
    total_bytes_relayed: int
    active_workers: int
    avg_bandwidth_mbps: float
    avg_latency_ms: float
    success_rate: float
    proof_count: int
    worker_contributions: Dict[str, int]
    merkle_root: str
    signature: str
    timestamp: float


class WorkerPaymentProof(BaseModel):
    """Individual worker payment with merkle proof."""
    worker_id: str
    worker_hotkey: str
    bytes_relayed: int
    amount_earned: int
    merkle_proof: list  # List of hex hashes
    leaf_index: int


class PaymentProofSubmission(BaseModel):
    """Payment proof submission from orchestrator."""
    epoch: int
    orchestrator_hotkey: str
    orchestrator_uid: Optional[int] = None
    merkle_root: str
    total_distributed: int  # In nano units (1e-9)
    worker_count: int
    total_bytes_relayed: int
    worker_payments: list  # List of WorkerPaymentProof dicts
    signature: str


# =============================================================================
# Orchestrator Communication Endpoints
# =============================================================================

@app.post("/orchestrator/report")
async def receive_orchestrator_report(report: OrchestratorReport):
    """
    Receive epoch work report from Orchestrator.

    Orchestrators send periodic reports with:
    - Aggregated work metrics (tasks, bytes, bandwidth)
    - Worker contributions
    - Merkle root of all proofs
    - Signed payload for verification

    Validators use these reports to score Orchestrators.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    try:
        # Verify the report signature
        from core._beam_stubs import verify_hotkey_signature

        message = f"{report.orchestrator_hotkey}:{report.epoch}:{report.total_bytes_relayed}:{report.merkle_root}"

        # Convert signature from hex
        signature_bytes = bytes.fromhex(report.signature)

        is_valid = verify_hotkey_signature(
            message.encode(),
            signature_bytes,
            report.orchestrator_hotkey,
        )

        if not is_valid:
            logger.warning(f"Invalid signature from orchestrator {report.orchestrator_hotkey[:16]}")
            raise HTTPException(status_code=401, detail="Invalid signature")

        # Store the report for scoring
        if not hasattr(validator, "orchestrator_reports"):
            validator.orchestrator_reports = {}

        if report.orchestrator_hotkey not in validator.orchestrator_reports:
            validator.orchestrator_reports[report.orchestrator_hotkey] = []

        validator.orchestrator_reports[report.orchestrator_hotkey].append({
            "epoch": report.epoch,
            "total_tasks": report.total_tasks,
            "total_bytes_relayed": report.total_bytes_relayed,
            "active_workers": report.active_workers,
            "avg_bandwidth_mbps": report.avg_bandwidth_mbps,
            "success_rate": report.success_rate,
            "merkle_root": report.merkle_root,
            "timestamp": report.timestamp,
            "received_at": asyncio.get_event_loop().time(),
        })

        # Keep only last 100 reports per orchestrator
        validator.orchestrator_reports[report.orchestrator_hotkey] = \
            validator.orchestrator_reports[report.orchestrator_hotkey][-100:]

        logger.info(
            f"Received report from orchestrator {report.orchestrator_hotkey[:16]}: "
            f"epoch={report.epoch}, tasks={report.total_tasks}, bytes={report.total_bytes_relayed}"
        )

        return {"status": "accepted", "epoch": report.epoch}

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"Error processing orchestrator report: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@app.get("/orchestrator/reports")
async def get_orchestrator_reports():
    """Get all received orchestrator reports."""
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, "orchestrator_reports"):
        return {"reports": {}}

    return {"reports": validator.orchestrator_reports}


@app.get("/orchestrator/reports/{hotkey}")
async def get_orchestrator_reports_by_hotkey(hotkey: str):
    """Get reports for a specific orchestrator."""
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, "orchestrator_reports"):
        return {"reports": []}

    reports = validator.orchestrator_reports.get(hotkey, [])
    return {"orchestrator_hotkey": hotkey, "reports": reports}


# =============================================================================
# Payment Proof Endpoints
# =============================================================================

@app.post("/payment-proof/submit")
async def submit_payment_proof(submission: PaymentProofSubmission):
    """
    Receive payment proof from orchestrator.

    Orchestrators must submit merkle proofs of worker payments.
    Validators verify these proofs and apply penalties for:
    - Missing proofs
    - Invalid merkle proofs
    - Payment amounts not matching work done

    This ensures orchestrators actually pay their workers.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    try:
        # Verify signature
        from core._beam_stubs import verify_hotkey_signature

        message = f"{submission.orchestrator_hotkey}:{submission.epoch}:{submission.merkle_root}:{submission.total_distributed}"
        signature_bytes = bytes.fromhex(submission.signature)

        is_valid = verify_hotkey_signature(
            message.encode(),
            signature_bytes,
            submission.orchestrator_hotkey,
        )

        if not is_valid:
            logger.warning(f"Invalid signature on payment proof from {submission.orchestrator_hotkey[:16]}")
            raise HTTPException(status_code=401, detail="Invalid signature")

        # Check if payment verification service is available
        if hasattr(validator, "payment_service") and validator.payment_service:
            from services.payment_verification import PaymentProofSubmission as PPSubmission

            proof_submission = PPSubmission(
                epoch=submission.epoch,
                orchestrator_hotkey=submission.orchestrator_hotkey,
                orchestrator_uid=submission.orchestrator_uid,
                merkle_root=submission.merkle_root,
                total_distributed=submission.total_distributed,
                worker_count=submission.worker_count,
                total_bytes_relayed=submission.total_bytes_relayed,
                worker_payments=submission.worker_payments,
            )

            result = await validator.payment_service.submit_payment_proof(proof_submission)

            logger.info(
                f"Payment proof from {submission.orchestrator_hotkey[:16]}: "
                f"epoch={submission.epoch}, valid={result.is_valid}, "
                f"compliance={result.compliance_score:.2f}"
            )

            return {
                "status": "accepted",
                "epoch": submission.epoch,
                "is_valid": result.is_valid,
                "compliance_score": result.compliance_score,
                "notes": result.notes,
            }

        else:
            # Fallback: store proof without verification service
            if not hasattr(validator, "payment_proofs"):
                validator.payment_proofs = {}

            key = f"{submission.orchestrator_hotkey}:{submission.epoch}"
            validator.payment_proofs[key] = {
                "epoch": submission.epoch,
                "merkle_root": submission.merkle_root,
                "total_distributed": submission.total_distributed,
                "worker_count": submission.worker_count,
                "total_bytes_relayed": submission.total_bytes_relayed,
                "received_at": asyncio.get_event_loop().time(),
            }

            logger.info(
                f"Payment proof stored (no verification service): "
                f"{submission.orchestrator_hotkey[:16]}, epoch={submission.epoch}"
            )

            return {
                "status": "stored",
                "epoch": submission.epoch,
                "notes": "Payment verification service not available",
            }

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"Error processing payment proof: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@app.get("/payment-proof/status/{orchestrator_hotkey}")
async def get_payment_proof_status(orchestrator_hotkey: str, epochs: int = 10):
    """
    Get payment proof status for an orchestrator.

    Returns recent proof submissions and compliance score.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if hasattr(validator, "payment_service") and validator.payment_service:
        proofs = await validator.payment_service.db.get_recent_proofs(
            orchestrator_hotkey=orchestrator_hotkey,
            limit=epochs,
        )

        return {
            "orchestrator_hotkey": orchestrator_hotkey,
            "proofs": [
                {
                    "epoch": p.epoch,
                    "merkle_root": p.merkle_root,
                    "total_distributed": p.total_distributed,
                    "worker_count": p.worker_count,
                    "is_valid": p.is_valid,
                    "verified_at": p.verified_at.isoformat() if p.verified_at else None,
                    "penalty_applied": p.penalty_applied,
                }
                for p in proofs
            ],
        }

    elif hasattr(validator, "payment_proofs"):
        # Fallback to in-memory storage
        proofs = [
            v for k, v in validator.payment_proofs.items()
            if k.startswith(orchestrator_hotkey)
        ]
        return {
            "orchestrator_hotkey": orchestrator_hotkey,
            "proofs": proofs,
        }

    return {"orchestrator_hotkey": orchestrator_hotkey, "proofs": []}


@app.get("/payment-proof/compliance")
async def get_payment_compliance():
    """
    Get payment compliance scores for all orchestrators.

    Shows which orchestrators are submitting valid payment proofs.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if hasattr(validator, "payment_service") and validator.payment_service:
        # Get all known orchestrator hotkeys
        if hasattr(validator, "orchestrator_reports"):
            hotkeys = list(validator.orchestrator_reports.keys())
        else:
            hotkeys = []

        compliance = {}
        for hotkey in hotkeys:
            score = await validator.payment_service.get_compliance_score(
                orchestrator_hotkey=hotkey,
                current_epoch=validator.current_epoch if hasattr(validator, "current_epoch") else 0,
            )
            compliance[hotkey] = score

        return {"compliance_scores": compliance}

    return {"compliance_scores": {}, "note": "Payment verification service not available"}


# =============================================================================
# Cross-Verification Endpoints
# =============================================================================

class ProofSubmissionRequest(BaseModel):
    """Request to submit a Proof-of-Bandwidth to the registry"""
    task_id: str
    epoch: int
    worker_hotkey: str
    orchestrator_hotkey: str
    bytes_relayed: int
    bandwidth_mbps: float
    start_time_us: int
    end_time_us: int
    canary_proof: str
    merkle_path: list = []
    path_index: int = 0
    signature: str


class OrchestratorClaimRequest(BaseModel):
    """Request to register an orchestrator's claimed score"""
    epoch: int
    orchestrator_hotkey: str
    proof_id: str
    claimed_bandwidth: float
    claimed_reward: float


class CrossVerificationStatsResponse(BaseModel):
    """Cross-verification statistics"""
    current_epoch: int
    phase: str
    proofs_verified: int
    has_commitment: bool
    reveal_submitted: bool
    orchestrator_penalties: int
    verifier_penalties: int


@app.get("/cross-verify/stats")
async def get_cross_verification_stats():
    """
    Get cross-verification statistics.

    Shows current phase, verification counts, and penalty summary.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    # Check if cross-verification service is available
    if hasattr(validator, "cross_verification_service") and validator.cross_verification_service:
        stats = validator.cross_verification_service.get_stats()
        return stats

    return {
        "status": "not_initialized",
        "note": "Cross-verification service not available",
    }


@app.get("/cross-verify/phase")
async def get_cross_verification_phase(current_block: int):
    """
    Get current cross-verification phase.

    Args:
        current_block: Current block number
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if hasattr(validator, "cross_verification_service") and validator.cross_verification_service:
        phase_info = validator.cross_verification_service.get_current_phase(current_block)
        return {
            "epoch": phase_info.epoch,
            "phase": phase_info.phase.value,
            "phase_start_block": phase_info.phase_start_block,
            "phase_end_block": phase_info.phase_end_block,
            "current_block": phase_info.current_block,
            "blocks_remaining": phase_info.blocks_remaining,
            "is_active": phase_info.is_phase_active,
        }

    return {"error": "Cross-verification service not available"}


@app.post("/cross-verify/proof")
async def submit_proof_to_registry(request: ProofSubmissionRequest):
    """
    Submit a Proof-of-Bandwidth to the Proof Registry.

    Workers call this after completing bandwidth relay tasks.
    The proof is stored in the shared registry for cross-verification.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, "cross_verification_service") or not validator.cross_verification_service:
        raise HTTPException(status_code=503, detail="Cross-verification service not available")

    try:
        from core._beam_stubs import ProofSubmission, sha256
        import time

        # Generate proof ID
        submitted_at = int(time.time() * 1_000_000)
        task_id_bytes = bytes.fromhex(request.task_id) if len(request.task_id) == 64 else sha256(request.task_id.encode())

        proof = ProofSubmission(
            proof_id=ProofSubmission.generate_proof_id(
                task_id_bytes,
                request.worker_hotkey,
                submitted_at,
            ),
            task_id=task_id_bytes,
            epoch=request.epoch,
            worker_hotkey=request.worker_hotkey,
            orchestrator_hotkey=request.orchestrator_hotkey,
            submitted_at=submitted_at,
            start_time_us=request.start_time_us,
            end_time_us=request.end_time_us,
            bytes_relayed=request.bytes_relayed,
            bandwidth_mbps=request.bandwidth_mbps,
            canary_proof=bytes.fromhex(request.canary_proof),
            merkle_path=[bytes.fromhex(p) for p in request.merkle_path] if request.merkle_path else [],
            path_index=request.path_index,
            signature=bytes.fromhex(request.signature),
        )

        success, message = await validator.cross_verification_service.submit_proof(proof)

        if success:
            return {
                "success": True,
                "proof_id": proof.proof_id.hex(),
                "message": message,
            }
        else:
            raise HTTPException(status_code=400, detail=message)

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"Error submitting proof: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@app.post("/cross-verify/claim")
async def register_orchestrator_claim(request: OrchestratorClaimRequest):
    """
    Register an orchestrator's claimed score for a proof.

    Orchestrators call this to record what they claim a worker's
    proof is worth. Cross-verification will compare against this.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, "cross_verification_service") or not validator.cross_verification_service:
        raise HTTPException(status_code=503, detail="Cross-verification service not available")

    try:
        await validator.cross_verification_service.register_orchestrator_claim(
            epoch=request.epoch,
            orchestrator_hotkey=request.orchestrator_hotkey,
            proof_id=bytes.fromhex(request.proof_id),
            claimed_bandwidth=request.claimed_bandwidth,
            claimed_reward=request.claimed_reward,
        )

        return {"success": True, "message": "Claim registered"}

    except Exception as e:
        logger.error(f"Error registering claim: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@app.get("/cross-verify/penalties/{epoch}")
async def get_epoch_penalties(epoch: int):
    """
    Get penalty summary for an epoch.

    Returns penalties applied to orchestrators based on cross-verification.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, "cross_verification_service") or not validator.cross_verification_service:
        raise HTTPException(status_code=503, detail="Cross-verification service not available")

    summary = validator.cross_verification_service.get_epoch_penalty_summary(epoch)

    if not summary:
        return {"epoch": epoch, "status": "not_available"}

    return {
        "epoch": summary.epoch,
        "orchestrators_penalized": summary.orchestrators_penalized,
        "avg_orchestrator_multiplier": summary.avg_orchestrator_multiplier,
        "verifiers_penalized": summary.verifiers_penalized,
        "avg_verifier_multiplier": summary.avg_verifier_multiplier,
        "total_penalty_redirect_percent": summary.total_penalty_redirect_percent,
        "orchestrator_details": {
            orch: {
                "penalty_multiplier": p.penalty_multiplier,
                "avg_deviation_percent": p.avg_deviation_percent,
                "inflation_count": p.inflation_count,
                "deflation_count": p.deflation_count,
                "consecutive_violations": p.consecutive_violations,
                "reason": p.penalty_reason,
            }
            for orch, p in summary.orchestrator_penalties.items()
        },
    }


@app.get("/cross-verify/penalty/{orchestrator_hotkey}")
async def get_orchestrator_penalty(orchestrator_hotkey: str, epoch: Optional[int] = None):
    """
    Get penalty for a specific orchestrator.

    Args:
        orchestrator_hotkey: The orchestrator's hotkey
        epoch: Epoch number (default: current epoch)
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, "cross_verification_service") or not validator.cross_verification_service:
        raise HTTPException(status_code=503, detail="Cross-verification service not available")

    if epoch is None:
        epoch = validator.cross_verification_service._current_epoch

    penalty = validator.cross_verification_service.get_orchestrator_penalty(epoch, orchestrator_hotkey)

    if not penalty:
        return {
            "orchestrator_hotkey": orchestrator_hotkey,
            "epoch": epoch,
            "penalty_multiplier": 1.0,
            "status": "no_penalty_data",
        }

    return {
        "orchestrator_hotkey": orchestrator_hotkey,
        "epoch": penalty.epoch,
        "penalty_multiplier": penalty.penalty_multiplier,
        "avg_deviation_percent": penalty.avg_deviation_percent,
        "max_deviation_percent": penalty.max_deviation_percent,
        "inflation_count": penalty.inflation_count,
        "deflation_count": penalty.deflation_count,
        "consecutive_violations": penalty.consecutive_violations,
        "escalation_level": penalty.escalation_level,
        "reason": penalty.penalty_reason,
    }


@app.get("/cross-verify/multipliers")
async def get_all_penalty_multipliers(epoch: Optional[int] = None):
    """
    Get all penalty multipliers for an epoch.

    Used by weight setting to adjust orchestrator weights.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, "cross_verification_service") or not validator.cross_verification_service:
        return {"multipliers": {}, "note": "Cross-verification service not available"}

    if epoch is None:
        epoch = validator.cross_verification_service._current_epoch

    multipliers = validator.cross_verification_service.get_all_penalty_multipliers(epoch)

    return {
        "epoch": epoch,
        "multipliers": multipliers,
        "count": len(multipliers),
    }


# =============================================================================
# Analytics API Endpoints (Public Dashboard)
# =============================================================================

@app.get("/analytics/network")
async def get_network_analytics():
    """
    Get high-level network analytics for public dashboard.

    Returns aggregated network statistics suitable for visualization.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    # Get orchestrator data
    orchestrators = []
    total_bandwidth = 0.0
    healthy_count = 0

    if hasattr(validator, 'orchestrator_manager'):
        for orch in validator.orchestrator_manager.get_all_orchestrators():
            is_healthy = True
            sla_multiplier = 1.0
            bandwidth = 0.0

            if orch.sla_state and orch.sla_state.score:
                sla_multiplier = orch.sla_state.score.effective_multiplier
                is_healthy = sla_multiplier > 0.5
                if orch.sla_state.metrics:
                    bandwidth = orch.sla_state.metrics.bandwidth_mbps

            if is_healthy:
                healthy_count += 1

            total_bandwidth += bandwidth

            orchestrators.append({
                "uid": orch.uid,
                "hotkey": orch.hotkey[:8] + "..." + orch.hotkey[-4:] if orch.hotkey else "unknown",
                "is_healthy": is_healthy,
                "sla_multiplier": round(sla_multiplier, 4),
                "bandwidth_mbps": round(bandwidth, 2),
                "worker_count": getattr(orch, 'worker_count', 0),
                "is_subnet_owned": getattr(orch, 'is_subnet_owned', False),
            })

    # Get Sybil detection stats
    sybil_stats = {"tracked_entities": 0, "suspicious_count": 0}
    if hasattr(validator, 'sybil_detector'):
        stats = validator.sybil_detector.get_statistics()
        sybil_stats = {
            "tracked_entities": stats.get("tracked_entities", 0),
            "suspicious_count": stats.get("suspicious_entities", 0),
            "unique_ips": stats.get("unique_ips", 0),
        }

    # Get health status
    health_status = "healthy"
    if hasattr(validator, 'health_monitor') and validator.health_monitor:
        health_status = validator.health_monitor.get_status().value

    return {
        "network": {
            "validator_hotkey": validator.hotkey[:8] + "..." + validator.hotkey[-4:] if validator.hotkey else "unknown",
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
        "orchestrator_list": sorted(orchestrators, key=lambda x: x["sla_multiplier"], reverse=True),
    }


@app.get("/analytics/orchestrators")
async def get_orchestrator_analytics():
    """
    Get detailed orchestrator analytics with SLA breakdown.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    orchestrators = []

    if hasattr(validator, 'orchestrator_manager'):
        for orch in validator.orchestrator_manager.get_all_orchestrators():
            orch_data = {
                "uid": orch.uid,
                "hotkey": orch.hotkey,
                "is_subnet_owned": getattr(orch, 'is_subnet_owned', False),
                "worker_count": getattr(orch, 'worker_count', 0),
            }

            # Add SLA metrics
            if orch.sla_state and orch.sla_state.score:
                score = orch.sla_state.score
                metrics = orch.sla_state.metrics or {}

                orch_data["sla"] = {
                    "effective_multiplier": round(score.effective_multiplier, 4),
                    "combined_multiplier": round(score.combined_multiplier, 4),
                    "penalty_percent": round(score.penalty_redirect_percent, 2),
                    "in_grace_period": score.in_grace_period,
                    "violations": [v.value for v in score.violations] if score.violations else [],
                    "multipliers": {
                        "uptime": round(score.multipliers.uptime, 4),
                        "bandwidth": round(score.multipliers.bandwidth, 4),
                        "latency": round(score.multipliers.latency, 4),
                        "jitter": round(score.multipliers.jitter, 4),
                        "acceptance": round(score.multipliers.acceptance, 4),
                        "success": round(score.multipliers.success, 4),
                    },
                }

                if hasattr(metrics, 'uptime_percent'):
                    orch_data["metrics"] = {
                        "uptime_percent": round(metrics.uptime_percent, 2),
                        "bandwidth_mbps": round(metrics.bandwidth_mbps, 2),
                        "latency_p95_ms": round(metrics.latency_p95_ms, 2),
                        "jitter_ms": round(getattr(metrics, 'jitter_ms', 0.0), 2),
                        "acceptance_rate": round(metrics.acceptance_rate_percent, 2),
                        "success_rate": round(metrics.success_rate_percent, 2),
                        "sample_count": metrics.sample_count,
                    }
            else:
                orch_data["sla"] = None
                orch_data["metrics"] = None

            # Add cross-verification penalty if available
            if hasattr(validator, 'cross_verification_service') and validator.cross_verification_service:
                cv_mult = validator.cross_verification_service.get_penalty_multiplier(
                    validator.cross_verification_service._current_epoch,
                    orch.hotkey
                )
                orch_data["cv_multiplier"] = round(cv_mult, 4)

            # Add Sybil penalty if available
            if hasattr(validator, '_get_sybil_penalty_multipliers'):
                sybil_mults = validator._get_sybil_penalty_multipliers()
                sybil_mult = sybil_mults.get(orch.hotkey, 1.0)
                orch_data["sybil_multiplier"] = round(sybil_mult, 4)

            orchestrators.append(orch_data)

    return {
        "count": len(orchestrators),
        "orchestrators": sorted(orchestrators, key=lambda x: x.get("sla", {}).get("effective_multiplier", 0) if x.get("sla") else 0, reverse=True),
    }


@app.get("/analytics/leaderboard")
async def get_leaderboard():
    """
    Get orchestrator leaderboard sorted by performance.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    leaderboard = []

    if hasattr(validator, 'orchestrator_manager'):
        for orch in validator.orchestrator_manager.get_all_orchestrators():
            if orch.is_subnet_owned:
                continue  # Skip subnet-owned orchestrator

            effective_mult = 1.0
            bandwidth = 0.0
            latency = 0.0

            if orch.sla_state and orch.sla_state.score:
                effective_mult = orch.sla_state.score.effective_multiplier
                if orch.sla_state.metrics:
                    bandwidth = orch.sla_state.metrics.bandwidth_mbps
                    latency = orch.sla_state.metrics.latency_p95_ms

            # Calculate composite score (weighted combination)
            # Higher bandwidth and lower latency = better
            score = effective_mult * 100

            leaderboard.append({
                "rank": 0,  # Will be set after sorting
                "uid": orch.uid,
                "hotkey_short": orch.hotkey[:8] + "..." + orch.hotkey[-4:] if orch.hotkey else "unknown",
                "score": round(score, 2),
                "sla_multiplier": round(effective_mult, 4),
                "bandwidth_mbps": round(bandwidth, 2),
                "latency_p95_ms": round(latency, 2),
                "worker_count": getattr(orch, 'worker_count', 0),
            })

    # Sort by score and assign ranks
    leaderboard.sort(key=lambda x: x["score"], reverse=True)
    for i, entry in enumerate(leaderboard):
        entry["rank"] = i + 1

    return {
        "updated_at": datetime.utcnow().isoformat(),
        "leaderboard": leaderboard[:50],  # Top 50
    }


@app.get("/analytics/history")
async def get_weight_history(limit: int = 24):
    """
    Get weight setting history for trend analysis.
    """
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
    """
    Get Sybil detection analytics.
    """
    if not validator:
        raise HTTPException(status_code=503, detail="Validator not initialized")

    if not hasattr(validator, 'sybil_detector'):
        return {"error": "Sybil detector not available"}

    detector = validator.sybil_detector
    stats = detector.get_statistics()

    # Get suspicious entities
    suspicious = detector.get_suspicious_entities()
    suspicious_list = []
    for hotkey, result in suspicious[:20]:  # Top 20
        suspicious_list.append({
            "hotkey_short": hotkey[:8] + "..." + hotkey[-4:] if hotkey else "unknown",
            "violations": [v.value for v in result.violations],
            "confidence": round(result.confidence, 2),
            "penalty_multiplier": round(result.penalty_multiplier, 2),
        })

    return {
        "stats": stats,
        "suspicious_entities": suspicious_list,
    }


# Import datetime for analytics endpoints
from datetime import datetime


# =============================================================================
# Main
# =============================================================================

def main():
    """Main entry point"""
    settings = get_settings()

    # Handle signals
    def signal_handler(sig, frame):
        logger.info("Shutdown signal received")
        sys.exit(0)

    signal.signal(signal.SIGINT, signal_handler)
    signal.signal(signal.SIGTERM, signal_handler)

    # Auto-open log viewer in browser (disabled by default, set OPEN_LOG_VIEWER=true to enable)
    if os.environ.get("OPEN_LOG_VIEWER", "").lower() in ("true", "1", "yes"):
        import webbrowser
        import threading
        import time
        log_viewer_url = os.environ.get("LOG_VIEWER_URL", "http://localhost:8080/logs/")
        def open_logs():
            time.sleep(1.5)  # Wait for server to start
            webbrowser.open(log_viewer_url)
        threading.Thread(target=open_logs, daemon=True).start()

    # Run server
    uvicorn.run(
        app,
        host="0.0.0.0",
        port=settings.port,
        log_level=settings.log_level.lower(),
    )


if __name__ == "__main__":
    main()
