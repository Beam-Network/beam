"""
Weight Calculator (Validator-Side Port)

Pure-function port of BeamCore's exposure-linked weight formula.
Constants and logic MUST match BeamCore/src/services/weight_calculator.py
exactly — the params_hash enforces this at runtime.

Formula:
    raw_weight = exposure * quality * confidence * penalty

Where:
    exposure   = blended share of verified traffic (70% bytes + 30% proofs)
    quality    = composite score (40/25/20/15) with multiplicative trust gate
    confidence = per-orchestrator sqrt ramp on verified work
    penalty    = fraud penalty multiplier (1.0 = no penalty, 0.0 = fully zeroed)
"""

import hashlib
import json
import logging
from dataclasses import dataclass
from math import sqrt
from typing import Dict, List, Optional, Tuple

logger = logging.getLogger(__name__)

# ── Formula version ──────────────────────────────────────────────────
FORMULA_VERSION = "exposure_v1"

# ── Quality coefficients (unchanged) ─────────────────────────────────
BANDWIDTH_WEIGHT = 0.40
COMPLIANCE_WEIGHT = 0.25
VERIFICATION_WEIGHT = 0.20
SPOT_CHECK_WEIGHT = 0.15

# ── Exposure blend (verified work) ───────────────────────────────────
EXPOSURE_BYTES_WEIGHT = 0.70
EXPOSURE_PROOFS_WEIGHT = 0.30

# ── Multiplicative trust gate ────────────────────────────────────────
TRUST_THRESHOLD_COMPLIANCE = 0.5
TRUST_THRESHOLD_VERIFICATION = 0.5
TRUST_PENALTY = 0.5  # per dimension; stacks -> 0.25 if both fail

# ── Minimum compliance for any emissions ────────────────────────────
# Orchestrators below this threshold receive zero weight regardless of activity.
# New orchestrators with no task history default to compliance=1.0 (no penalty).
MIN_COMPLIANCE_FOR_EMISSIONS = 0.5

# ── Confidence (per-orchestrator, blended) ───────────────────────────
CONFIDENCE_PROOF_THRESHOLD = 10
CONFIDENCE_BYTES_THRESHOLD = 1_000_000_000  # 1 GB
CONFIDENCE_PROOFS_WEIGHT = 0.70
CONFIDENCE_BYTES_WEIGHT = 0.30

# ── Outflow penalty (traffic-selling deterrent) ──────────────────────
_OUTFLOW_ENABLED_DEFAULT = True
_OUTFLOW_PENALTY_PERCENTILE_DEFAULT = 0.75  # Top 25% sellers get penalized
_OUTFLOW_MAX_PENALTY_DEFAULT = 0.25  # Worst offender multiplier (0.25 = 75% traffic reduction)


def get_outflow_settings() -> tuple:
    """Get outflow settings (hardcoded defaults for validator)."""
    return (
        _OUTFLOW_ENABLED_DEFAULT,
        _OUTFLOW_PENALTY_PERCENTILE_DEFAULT,
        _OUTFLOW_MAX_PENALTY_DEFAULT,
    )


# ── Bittensor ────────────────────────────────────────────────────────
MAX_WEIGHT_UINT16 = 65535


def _get_tunable_params() -> dict:
    """All tunable constants as a dict (for hashing and serialization)."""
    outflow_enabled, outflow_percentile, outflow_max_penalty = get_outflow_settings()
    return {
        "BANDWIDTH_WEIGHT": BANDWIDTH_WEIGHT,
        "COMPLIANCE_WEIGHT": COMPLIANCE_WEIGHT,
        "CONFIDENCE_BYTES_THRESHOLD": CONFIDENCE_BYTES_THRESHOLD,
        "CONFIDENCE_BYTES_WEIGHT": CONFIDENCE_BYTES_WEIGHT,
        "CONFIDENCE_PROOF_THRESHOLD": CONFIDENCE_PROOF_THRESHOLD,
        "CONFIDENCE_PROOFS_WEIGHT": CONFIDENCE_PROOFS_WEIGHT,
        "EXPOSURE_BYTES_WEIGHT": EXPOSURE_BYTES_WEIGHT,
        "EXPOSURE_PROOFS_WEIGHT": EXPOSURE_PROOFS_WEIGHT,
        "OUTFLOW_ENABLED": outflow_enabled,
        "OUTFLOW_MAX_PENALTY": outflow_max_penalty,
        "OUTFLOW_PENALTY_PERCENTILE": outflow_percentile,
        "SPOT_CHECK_WEIGHT": SPOT_CHECK_WEIGHT,
        "TRUST_PENALTY": TRUST_PENALTY,
        "TRUST_THRESHOLD_COMPLIANCE": TRUST_THRESHOLD_COMPLIANCE,
        "TRUST_THRESHOLD_VERIFICATION": TRUST_THRESHOLD_VERIFICATION,
        "VERIFICATION_WEIGHT": VERIFICATION_WEIGHT,
    }


def get_params_hash() -> str:
    """SHA-256 of all tunable constants -- deterministic config identifier."""
    canonical = json.dumps(_get_tunable_params(), sort_keys=True)
    return hashlib.sha256(canonical.encode()).hexdigest()


def get_params_json() -> str:
    """JSON snapshot of all tunables for audit transparency."""
    return json.dumps(_get_tunable_params(), sort_keys=True)


def verify_params_hash(beamcore_hash: str) -> bool:
    """Compare local params_hash against BeamCore's. Log mismatch."""
    local_hash = get_params_hash()
    if local_hash != beamcore_hash:
        logger.error(
            f"params_hash MISMATCH: local={local_hash[:16]}... "
            f"beamcore={beamcore_hash[:16]}... — refusing exposure_v1"
        )
        return False
    return True


@dataclass
class SummaryInput:
    """Lightweight input matching the fields the formula actually reads.

    Fields map 1:1 to what the epoch summaries API returns:
        orchestrator_hotkey, orchestrator_uid,
        total_proofs_published, total_bytes_claimed,
        bandwidth_score, compliance_score, verification_rate, spot_check_rate
    """

    orchestrator_hotkey: str
    orchestrator_uid: int
    total_proofs_published: int
    total_bytes_claimed: int
    bandwidth_score: float  # quality component (40%)
    compliance_score: float  # quality component (25%)
    verification_rate: float  # quality component (20%) + exposure scaling
    spot_check_rate: float  # quality component (15%)


@dataclass
class ExposureDetail:
    """Per-orchestrator exposure computation intermediates."""

    effective_bytes: int
    effective_proofs: float
    bytes_share: float
    proofs_share: float
    verification_rate_clamped: float
    exposure: float


@dataclass
class OrchestratorWeight:
    """Computed weight for a single orchestrator with full breakdown."""

    uid: int
    hotkey: str
    # Final weights
    raw_score: float
    penalized_score: float
    normalized_weight: float
    uint16_weight: int
    # Exposure
    total_bytes_claimed: int
    total_proofs_published: int
    verification_rate_clamped: float
    effective_bytes: int
    effective_proofs: float
    bytes_share: float
    proofs_share: float
    exposure: float
    # Quality
    bandwidth_score: float
    compliance_score: float
    verification_rate: float
    spot_check_rate: float
    quality_raw: float
    trust_multiplier: float
    quality: float
    # Confidence
    confidence_proofs: float
    confidence_bytes: float
    confidence: float
    # Penalty
    penalty_multiplier: float


@dataclass
class WeightVector:
    """Complete weight vector for an epoch."""

    epoch: int
    uids: List[int]
    weights: List[float]  # Normalized (sum ~= 1.0)
    uint16_weights: List[int]  # For bittensor set_weights
    details: List[OrchestratorWeight]
    formula: str
    formula_version: str
    params_hash: str
    params_json: str


def _clamp_verification_rate(vr: Optional[float]) -> float:
    """Clamp verification_rate to [0, 1] to guard against bad DB values."""
    return max(0.0, min(vr or 0.0, 1.0))


def compute_exposure(summaries: List[SummaryInput]) -> Dict[str, ExposureDetail]:
    """
    Compute exposure (blended routing share) for each orchestrator.

    Uses verified work (effective = claimed * verification_rate) so that
    orchestrators with low verification rates get proportionally less exposure.

    Returns dict of hotkey -> ExposureDetail.
    """
    n = len(summaries)
    if n == 0:
        return {}

    equal_share = 1.0 / n

    # Compute effective (verified) values per orchestrator
    per_orch = {}
    total_eff_bytes = 0.0
    total_eff_proofs = 0.0

    for s in summaries:
        vr = _clamp_verification_rate(s.verification_rate)
        eff_bytes = int((s.total_bytes_claimed or 0) * vr)
        eff_proofs = (s.total_proofs_published or 0) * vr
        per_orch[s.orchestrator_hotkey] = (eff_bytes, eff_proofs, vr)
        total_eff_bytes += eff_bytes
        total_eff_proofs += eff_proofs

    # Compute blended exposure shares
    exposures = {}
    for s in summaries:
        eff_bytes, eff_proofs, vr = per_orch[s.orchestrator_hotkey]

        bytes_share = (eff_bytes / total_eff_bytes) if total_eff_bytes > 0 else equal_share
        proofs_share = (eff_proofs / total_eff_proofs) if total_eff_proofs > 0 else equal_share

        exposure = EXPOSURE_BYTES_WEIGHT * bytes_share + EXPOSURE_PROOFS_WEIGHT * proofs_share

        exposures[s.orchestrator_hotkey] = ExposureDetail(
            effective_bytes=eff_bytes,
            effective_proofs=eff_proofs,
            bytes_share=bytes_share,
            proofs_share=proofs_share,
            verification_rate_clamped=vr,
            exposure=exposure,
        )

    return exposures


def compute_quality(
    bandwidth_score: float,
    compliance_score: float,
    verification_rate: float,
    spot_check_rate: float,
) -> Tuple[float, float, float]:
    """
    Compute quality score with multiplicative trust gate.

    Returns (quality_final, quality_raw, trust_multiplier).
    """
    quality_raw = (
        BANDWIDTH_WEIGHT * bandwidth_score
        + COMPLIANCE_WEIGHT * compliance_score
        + VERIFICATION_WEIGHT * verification_rate
        + SPOT_CHECK_WEIGHT * spot_check_rate
    )

    trust_multiplier = 1.0
    if compliance_score < TRUST_THRESHOLD_COMPLIANCE:
        trust_multiplier *= TRUST_PENALTY
    if verification_rate < TRUST_THRESHOLD_VERIFICATION:
        trust_multiplier *= TRUST_PENALTY

    return quality_raw * trust_multiplier, quality_raw, trust_multiplier


def compute_confidence(
    total_proofs_published: int,
    total_bytes_claimed: int,
    verification_rate: float,
) -> Tuple[float, float, float]:
    """
    Compute per-orchestrator confidence using verified work.

    Blended sqrt ramp: 70% proof confidence + 30% bytes confidence.
    Each component is capped at 1.0.

    Returns (confidence, conf_proofs, conf_bytes).
    """
    vr = _clamp_verification_rate(verification_rate)
    eff_proofs = (total_proofs_published or 0) * vr
    eff_bytes = (total_bytes_claimed or 0) * vr

    conf_proofs = (
        min(sqrt(eff_proofs / CONFIDENCE_PROOF_THRESHOLD), 1.0)
        if CONFIDENCE_PROOF_THRESHOLD > 0
        else 1.0
    )
    conf_bytes = (
        min(sqrt(eff_bytes / CONFIDENCE_BYTES_THRESHOLD), 1.0)
        if CONFIDENCE_BYTES_THRESHOLD > 0
        else 1.0
    )

    confidence = CONFIDENCE_PROOFS_WEIGHT * conf_proofs + CONFIDENCE_BYTES_WEIGHT * conf_bytes
    return confidence, conf_proofs, conf_bytes


def compute_weights(
    summaries: List[SummaryInput],
    penalty_multipliers: Optional[Dict[str, float]] = None,
    epoch: int = 0,
) -> WeightVector:
    """
    Compute weight vector from epoch summaries.

    Formula: raw = exposure * quality * confidence * penalty
    Then normalize across all orchestrators and convert to uint16.

    Args:
        summaries: List of SummaryInput objects
        penalty_multipliers: Dict of hotkey -> multiplier (0.0-1.0).
            1.0 = no penalty, 0.0 = fully penalized.
        epoch: Epoch number for the weight vector.

    Returns:
        WeightVector with normalized weights and full breakdowns.
    """
    if penalty_multipliers is None:
        penalty_multipliers = {}

    params_hash = get_params_hash()
    params_json_str = get_params_json()

    # Step 1: Compute exposure shares (needs all summaries)
    exposure_details = compute_exposure(summaries)

    details: List[OrchestratorWeight] = []

    for s in summaries:
        bw = s.bandwidth_score or 0.0
        comp = s.compliance_score or 0.0
        verif = s.verification_rate or 0.0
        spot = s.spot_check_rate or 0.0

        exp_detail = exposure_details.get(s.orchestrator_hotkey)
        exposure_val = exp_detail.exposure if exp_detail else 0.0

        quality_final, quality_raw, trust_mult = compute_quality(bw, comp, verif, spot)
        confidence_val, conf_proofs, conf_bytes = compute_confidence(
            s.total_proofs_published or 0,
            s.total_bytes_claimed or 0,
            s.verification_rate,
        )
        penalty = penalty_multipliers.get(s.orchestrator_hotkey, 1.0)

        # Hard compliance gate: below MIN_COMPLIANCE_FOR_EMISSIONS = zero emissions.
        # Partial payers get nothing. New orchestrators with no task history
        # default to compliance=1.0 so they are not penalized before their first epoch.
        effective_comp = comp if comp >= MIN_COMPLIANCE_FOR_EMISSIONS else 0.0
        raw = exposure_val * quality_final * confidence_val * penalty * effective_comp
        raw = max(0.0, raw)

        details.append(
            OrchestratorWeight(
                uid=s.orchestrator_uid,
                hotkey=s.orchestrator_hotkey,
                raw_score=raw,
                penalized_score=raw,
                normalized_weight=0.0,
                uint16_weight=0,
                # Exposure
                total_bytes_claimed=s.total_bytes_claimed or 0,
                total_proofs_published=s.total_proofs_published or 0,
                verification_rate_clamped=(
                    exp_detail.verification_rate_clamped if exp_detail else 0.0
                ),
                effective_bytes=exp_detail.effective_bytes if exp_detail else 0,
                effective_proofs=exp_detail.effective_proofs if exp_detail else 0.0,
                bytes_share=exp_detail.bytes_share if exp_detail else 0.0,
                proofs_share=exp_detail.proofs_share if exp_detail else 0.0,
                exposure=exposure_val,
                # Quality
                bandwidth_score=bw,
                compliance_score=comp,
                verification_rate=verif,
                spot_check_rate=spot,
                quality_raw=quality_raw,
                trust_multiplier=trust_mult,
                quality=quality_final,
                # Confidence
                confidence_proofs=conf_proofs,
                confidence_bytes=conf_bytes,
                confidence=confidence_val,
                # Penalty
                penalty_multiplier=penalty,
            )
        )

    # Normalize
    total = sum(d.raw_score for d in details)
    if total > 0:
        for d in details:
            d.normalized_weight = d.raw_score / total
            d.uint16_weight = int(d.normalized_weight * MAX_WEIGHT_UINT16)
    elif details:
        equal = 1.0 / len(details)
        for d in details:
            d.normalized_weight = equal
            d.uint16_weight = int(equal * MAX_WEIGHT_UINT16)

    # Sort by UID for deterministic ordering
    details.sort(key=lambda d: d.uid)

    uids = [d.uid for d in details]
    weights = [d.normalized_weight for d in details]
    uint16_weights = [d.uint16_weight for d in details]

    formula = "exposure * quality * confidence * penalty"

    logger.info(
        f"Computed weights for epoch {epoch}: "
        f"{len(details)} orchestrators, total_raw={total:.6f}"
    )

    return WeightVector(
        epoch=epoch,
        uids=uids,
        weights=weights,
        uint16_weights=uint16_weights,
        details=details,
        formula=formula,
        formula_version=FORMULA_VERSION,
        params_hash=params_hash,
        params_json=params_json_str,
    )
