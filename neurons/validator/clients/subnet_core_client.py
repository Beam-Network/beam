"""
BeamCore HTTP client for the validator neuron.

Authentication
--------------
All routes authenticate with validator hotkey signatures via the standard header fields
(`X-Validator-Hotkey`, `X-Validator-Signature`, `X-Validator-Timestamp`, `X-Validator-Nonce`,
`X-Validator-Action`).

`set_weights` and recommended-weight logic read the signed **epoch-summary** response. PoB endpoints cover
proof listing and verification.
"""

import logging
import secrets
import time
from dataclasses import dataclass
from typing import Any, Dict, Optional

import httpx

logger = logging.getLogger(__name__)


def build_signed_auth_headers(
    wallet,
    hotkey: str,
    action: str = "request",
) -> Dict[str, str]:
    """Signed headers for validator BeamCore routes."""
    timestamp = int(time.time())
    nonce = secrets.token_hex(16)
    message = f"validator_auth:{hotkey}:{timestamp}:{action}:{nonce}"
    signature = wallet.hotkey.sign(message.encode("utf-8"))
    return {
        "X-Validator-Hotkey": hotkey,
        "X-Validator-Signature": signature.hex(),
        "X-Validator-Timestamp": str(timestamp),
        "X-Validator-Nonce": nonce,
        "X-Validator-Action": action,
    }


class SubnetCoreClient:
    """Uses `x-api-key` for PoB; signed validator headers for heartbeat and epoch-summary."""

    def __init__(
        self,
        base_url: str,
        validator_hotkey: str,
        wallet=None,
        api_key: Optional[str] = None,
        timeout: float = 30.0,
    ):
        self.base_url = base_url.rstrip("/")
        self.validator_hotkey = validator_hotkey
        self.wallet = wallet
        self._api_key = (api_key or "").strip() or None
        self.timeout = timeout
        self._client: Optional[httpx.AsyncClient] = None

    def _signed_headers(self, action: str) -> Dict[str, str]:
        if not self.wallet:
            raise RuntimeError("wallet required for signed BeamCore validator routes")
        return build_signed_auth_headers(self.wallet, self.validator_hotkey, action)

    async def _get_client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=self.timeout)
        return self._client

    async def _request(
        self,
        method: str,
        path: str,
        action: str = "request",
        **kwargs,
    ) -> httpx.Response:
        client = await self._get_client()
        path_only = path.split("?", 1)[0]
        lower = path_only.lower()

        # All routes use validator hotkey signature
        try:
            headers = self._signed_headers(action) if self.wallet else {"X-Validator-Hotkey": self.validator_hotkey}
        except RuntimeError:
            headers = {"X-Validator-Hotkey": self.validator_hotkey}
        if "headers" in kwargs:
            headers.update(kwargs.pop("headers"))
        return await client.request(method, f"{self.base_url}{path}", headers=headers, **kwargs)

    async def close(self):
        if self._client:
            await self._client.aclose()
            self._client = None

    async def get_unverified_proofs(
        self,
        epoch: Optional[int] = None,
        limit: int = 100,
    ) -> Dict[str, Any]:
        params: Dict[str, Any] = {"limit": limit}
        if epoch is not None:
            params["epoch"] = epoch
        response = await self._request(
            "GET",
            "/pob/unverified",
            action="get_unverified_proofs",
            params=params,
        )
        response.raise_for_status()
        return response.json()

    async def verify_proof(
        self,
        proof_id: str,
        passed: bool,
        signature_valid: bool = False,
        timing_valid: bool = False,
        bandwidth_valid: bool = False,
        canary_valid: bool = False,
        geo_valid: bool = False,
        verification_notes: Optional[str] = None,
        measured_latency_ms: Optional[float] = None,
    ) -> Dict[str, Any]:
        notes: list = [
            f"signature_valid={signature_valid}",
            f"timing_valid={timing_valid}",
            f"bandwidth_valid={bandwidth_valid}",
            f"canary_valid={canary_valid}",
            f"geo_valid={geo_valid}",
        ]
        if measured_latency_ms is not None:
            notes.append(f"measured_latency_ms={measured_latency_ms:.3f}")
        if verification_notes:
            notes.append(verification_notes)

        response = await self._request(
            "POST",
            f"/pob/{proof_id}/verify",
            action="verify_proof",
            json={
                "passed": passed,
                "validator_hotkey": self.validator_hotkey,
                "notes": "; ".join(notes),
            },
        )
        response.raise_for_status()
        return response.json()

    async def get_latest_data_epoch(self) -> Optional[int]:
        try:
            response = await self._request(
                "GET",
                "/pob/latest-epoch",
                action="get_latest_data_epoch",
            )
            response.raise_for_status()
            return response.json().get("epoch")
        except Exception as exc:
            logger.warning("Failed to get latest data epoch: %s", exc)
            return None

    async def get_proofs_from_subnetcore(
        self,
        epoch: Optional[int] = None,
        orchestrator_hotkey: Optional[str] = None,
        worker_id: Optional[str] = None,
        limit: int = 100,
        status: Optional[str] = None,
    ) -> Dict[str, Any]:
        params: Dict[str, Any] = {"limit": limit}
        if epoch is not None:
            params["epoch"] = epoch
        if orchestrator_hotkey:
            params["orchestrator_hotkey"] = orchestrator_hotkey
        if worker_id:
            params["worker_id"] = worker_id
        if status:
            params["status"] = status
        response = await self._request(
            "GET",
            "/pob",
            action="get_proofs",
            params=params,
        )
        response.raise_for_status()
        return response.json()

    async def get_proof(self, task_id: str) -> Dict[str, Any]:
        """Get a specific proof by task ID."""
        try:
            response = await self._request(
                "GET",
                f"/pob/proof/{task_id}",
                action="get_proof",
            )
            response.raise_for_status()
            return response.json()
        except httpx.HTTPStatusError as e:
            logger.error(f"HTTP error fetching proof: {e.response.status_code}")
            raise
        except Exception as e:
            logger.error(f"Error fetching proof: {e}")
            raise

    async def get_latest_epoch_summary(self) -> Dict[str, Any]:
        """Latest epoch summary from BeamCore (signed validator request)."""
        response = await self._request(
            "GET",
            "/Validator/epoch-summary/latest-epoch",
            action="epoch_summary",
        )
        response.raise_for_status()
        return response.json()

    async def submit_weight_proof(
        self,
        epoch: int,
        block_number: int,
        netuid: int,
        uids: list,
        weights: list,
        formula_version: Optional[str] = None,
        params_hash: Optional[str] = None,
        tx_hash: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Record a successful on-chain weight set so the dashboard can reflect it."""
        body = {
            "epoch": epoch,
            "block_number": block_number,
            "netuid": netuid,
            "uids": uids,
            "weights": weights,
            "formula_version": formula_version,
            "params_hash": params_hash,
            "tx_hash": tx_hash,
        }
        response = await self._request(
            "POST",
            "/validators/weights/proof",
            action="submit_weight_proof",
            json={k: v for k, v in body.items() if v is not None},
        )
        response.raise_for_status()
        return response.json()

    async def submit_heartbeat(
        self,
        validator_uid: int,
        status: str = "online",
        last_epoch_scored: Optional[int] = None,
        health_info: Optional[dict] = None,
        external_url: Optional[str] = None,
    ) -> Dict[str, Any]:
        response = await self._request(
            "POST",
            "/validators/heartbeat",
            action="heartbeat",
            json={
                "validator_hotkey": self.validator_hotkey,
                "validator_uid": validator_uid,
                "status": status,
                "timestamp": int(time.time() * 1e6),
                "last_epoch_scored": last_epoch_scored,
                "health_info": health_info,
                "external_url": external_url,
            },
        )
        response.raise_for_status()
        return response.json()


_client: Optional[SubnetCoreClient] = None


def get_subnet_core_client() -> Optional[SubnetCoreClient]:
    return _client


def init_subnet_core_client(
    base_url: str,
    validator_hotkey: str,
    wallet=None,
    api_key: Optional[str] = None,
    timeout: float = 30.0,
) -> SubnetCoreClient:
    global _client
    _client = SubnetCoreClient(base_url, validator_hotkey, wallet, api_key=api_key, timeout=timeout)
    return _client


async def close_subnet_core_client():
    global _client
    if _client:
        await _client.close()
        _client = None
