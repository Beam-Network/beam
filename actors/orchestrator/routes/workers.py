"""
Worker WebSocket gateway route.

Workers connect via: GET /ws/{worker_id}?api_key=<beamcore_api_key>

The orchestrator validates the key against BeamCore, then registers the worker
session in WorkerGateway for task_offer delivery and result relay.
"""

import asyncio
import logging

import httpx
from fastapi import APIRouter, WebSocket, WebSocketDisconnect, status
from fastapi.websockets import WebSocketState

from core.orchestrator import get_orchestrator

logger = logging.getLogger(__name__)

router = APIRouter(tags=["workers"])

_VALIDATE_TIMEOUT = 5.0  # seconds — how long to wait for BeamCore key validation


async def _validate_worker_api_key(core_url: str, worker_id: str, api_key: str) -> bool:
    """Return True iff BeamCore confirms this api_key belongs to worker_id."""
    try:
        async with httpx.AsyncClient(timeout=_VALIDATE_TIMEOUT) as client:
            resp = await client.get(
                f"{core_url.rstrip('/')}/workers/{worker_id}",
                headers={"x-api-key": api_key},
            )
            if resp.status_code == 200:
                data = resp.json()
                return data.get("worker_id") == worker_id
            return False
    except Exception as exc:
        logger.warning("BeamCore key validation failed for %s: %s", worker_id, exc)
        return False


@router.websocket("/ws/{worker_id}")
async def worker_ws(websocket: WebSocket, worker_id: str) -> None:
    orchestrator = get_orchestrator()
    if orchestrator is None:
        await websocket.close(code=status.WS_1013_TRY_AGAIN_LATER)
        return

    gateway = getattr(orchestrator, "worker_gateway", None)
    if gateway is None:
        await websocket.close(code=status.WS_1013_TRY_AGAIN_LATER)
        return

    # --- Auth ---
    api_key = websocket.query_params.get("api_key") or ""
    if not api_key:
        await websocket.close(code=status.WS_1008_POLICY_VIOLATION)
        return

    core_url = orchestrator.settings.core_server_url
    valid = await _validate_worker_api_key(core_url, worker_id, api_key)
    if not valid:
        logger.warning("Worker %s: API key validation failed", worker_id)
        await websocket.close(code=status.WS_1008_POLICY_VIOLATION)
        return

    # --- Capacity check ---
    if gateway.is_full() and worker_id not in gateway.worker_ids:
        await websocket.close(
            code=status.WS_1013_TRY_AGAIN_LATER,
        )
        return

    await websocket.accept()

    if not gateway.connect(worker_id, websocket):
        await websocket.close(code=status.WS_1013_TRY_AGAIN_LATER)
        return

    try:
        await websocket.send_text('{"type":"connected"}')

        while True:
            raw = await websocket.receive_text()
            await gateway.handle_worker_message(worker_id, raw)

    except WebSocketDisconnect:
        pass
    except Exception as exc:
        logger.warning("Worker WS error for %s: %s", worker_id, exc)
    finally:
        gateway.disconnect(worker_id)
        if websocket.client_state != WebSocketState.DISCONNECTED:
            try:
                await websocket.close()
            except Exception:
                pass
