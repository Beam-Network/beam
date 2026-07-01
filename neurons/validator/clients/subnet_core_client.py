"""
BeamCore HTTP client for the validator neuron.

`POST /validators/heartbeat`, `POST /validators/weights/proof`, and
`GET /Validator/epoch-summary/latest-epoch` authenticate with validator hotkey
signatures via the standard header fields. UID and network configuration routes
use the validator API key when one is configured.
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


@dataclass
class UIDRanges:
    public_orchestrator_uid_start: int
    public_orchestrator_uid_end: int
    max_orchestrators: int

    def is_valid_orchestrator_uid(self, uid: int) -> bool:
        return self.public_orchestrator_uid_start <= uid <= self.public_orchestrator_uid_end


class SubnetCoreClient:
    """Uses signed validator headers for heartbeat, weights, and epoch summaries."""

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


        if lower.startswith("/validators/heartbeat") or lower.startswith("/validator/epoch-summary"):
            headers = self._signed_headers(action)
            if "headers" in kwargs:
                headers.update(kwargs.pop("headers"))
            req_kwargs = dict(kwargs)
            return await client.request(method, f"{self.base_url}{path}", headers=headers, **req_kwargs)

        # Default (e.g. legacy paths): try signature if wallet present, else minimal
        try:
            headers = self._signed_headers(action) if self.wallet else {"X-Validator-Hotkey": self.validator_hotkey}
        except RuntimeError:
            headers = {"X-Validator-Hotkey": self.validator_hotkey}
        if self._api_key:
            headers.setdefault("x-api-key", self._api_key)
        if "headers" in kwargs:
            headers.update(kwargs.pop("headers"))
        return await client.request(method, f"{self.base_url}{path}", headers=headers, **kwargs)

    async def close(self):
        if self._client:
            await self._client.aclose()
            self._client = None


    async def get_latest_epoch_summary(self) -> Dict[str, Any]:
        """Latest epoch summary from BeamCore (signed validator request)."""
        response = await self._request(
            "GET",
            "/Validator/epoch-summary/latest-epoch",
            action="epoch_summary",
        )
        response.raise_for_status()
        return response.json()

    async def get_uid_ranges(self) -> Optional[UIDRanges]:
        client = await self._get_client()
        headers: Dict[str, str] = {}
        if self._api_key:
            headers["x-api-key"] = self._api_key
        try:
            response = await client.get(f"{self.base_url}/config/uid-ranges", headers=headers)
            response.raise_for_status()
            data = response.json()
            return UIDRanges(
                public_orchestrator_uid_start=data["public_orchestrator_uid_start"],
                public_orchestrator_uid_end=data["public_orchestrator_uid_end"],
                max_orchestrators=data["max_orchestrators"],
            )
        except Exception as exc:
            logger.error("Error fetching UID ranges: %s", exc)
            return None

    async def get_network_config(self) -> Optional[dict]:
        client = await self._get_client()
        headers: Dict[str, str] = {}
        if self._api_key:
            headers["x-api-key"] = self._api_key
        try:
            response = await client.get(f"{self.base_url}/config/network", headers=headers)
            response.raise_for_status()
            return response.json()
        except Exception as exc:
            logger.warning("Error fetching network config: %s", exc)
            return None

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
                "needs_api_key": self._api_key is None,
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
