"""External client bootstrap and optional local development helpers."""

import logging
import time

from .models import Worker, WorkerStatus

logger = logging.getLogger(__name__)


def _generate_worker_id(orchestrator, hotkey: str, ip: str, port: int) -> str:
    """Generate unique worker ID."""
    import hashlib

    data = f"{hotkey}:{ip}:{port}:{time.time()}"
    return hashlib.sha256(data.encode()).hexdigest()[:16]

async def _init_subnet_core_client(
    orchestrator,
    subnet_core_available: bool,
    init_client_fn,
) -> None:
    """Initialize SubnetCoreClient for API-based data operations."""
    if not subnet_core_available:
        logger.warning(
            "SubnetCoreClient not available - data will not be persisted to BeamCore"
        )
        return

    if not orchestrator.settings.core_server_url:
        logger.warning(
            "CORE_SERVER_URL not configured - data will not be persisted to BeamCore"
        )
        return

    try:
        signer = None
        if orchestrator.wallet and hasattr(orchestrator.wallet, "hotkey"):
            signer = orchestrator.wallet.hotkey
        orchestrator.subnet_core_client = init_client_fn(
            base_url=orchestrator.settings.core_server_url,
            ws_base_url=orchestrator.settings.orch_gateway_url,
            orchestrator_hotkey=orchestrator.hotkey or "unknown",
            orchestrator_uid=orchestrator.our_uid or 0,
            signer=signer,
        )
        logger.info(
            "SubnetCoreClient initialized: http=%s nats=%s",
            orchestrator.settings.core_server_url,
            orchestrator.settings.orch_gateway_url,
        )

        # NATS control task offer batches drive worker routing.
        orchestrator.subnet_core_client.set_worker_update_handler(orchestrator._worker_mgr.handle_worker_update)

        # Wire the in-process worker gateway.
        orchestrator.subnet_core_client.set_worker_gateway(orchestrator.worker_gateway)

        # Configure registration message sent on NATS control connect
        import socket as _socket

        try:
            _s = _socket.socket(_socket.AF_INET, _socket.SOCK_DGRAM)
            _s.connect(("8.8.8.8", 80))
            local_ip = orchestrator.settings.external_ip or _s.getsockname()[0]
            _s.close()
        except Exception:
            local_ip = orchestrator.settings.external_ip or "127.0.0.1"
        orch_url = f"http://{local_ip}:{orchestrator.settings.api_port}"
        # gateway_url: explicit orchestrator-owned worker gateway override takes priority;
        # otherwise derive from the orchestrator's own HTTP address.
        gateway_url = (
            orchestrator.settings.worker_gateway_url
            or f"http://{local_ip}:{orchestrator.settings.api_port}"
        )
        orchestrator.subnet_core_client.set_registration_config(
            url=orch_url,
            region=orchestrator.settings.region,
            max_workers=orchestrator.settings.max_workers,
            uid=orchestrator.our_uid,
            fee_percentage=orchestrator.settings.fee_percentage,
            gateway_url=gateway_url,
        )
        orchestrator.subnet_core_client.prime_ready_state(bool(orchestrator.settings.ready))
        logger.info(
            "WS registration config set: url=%s region=%s gateway_url=%s",
            orch_url,
            orchestrator.settings.region,
            gateway_url,
        )

        # Start NATS control connection for real-time notifications and
        # orchestrator control-plane requests.
        await orchestrator.subnet_core_client.start_polling()
        logger.info("SubnetCore NATS control connection started")

    except Exception as e:
        logger.warning(f"Failed to initialize SubnetCoreClient: {e}")
        orchestrator.subnet_core_client = None

# SubnetCoreClient receives BeamCore task offer batches and routes them
# to connected local workers.

async def _init_orch_manager(orchestrator) -> None:
    """Initialize the orchestrator manager for incentive mechanism."""
    try:
        from beam.orchestrator import OrchestratorManager

        orchestrator.orch_manager = OrchestratorManager()
        logger.info("Orchestrator manager initialized (in-memory mode)")
    except ImportError:
        # OrchestratorManager is optional - not needed for normal operation
        orchestrator.orch_manager = None
    except Exception as e:
        logger.error(f"Failed to initialize orchestrator manager: {e}")
        orchestrator.orch_manager = None

async def _add_local_mock_worker(orchestrator) -> None:
    """Add a mock worker for local mode bandwidth challenges."""
    worker_hotkey = (
        orchestrator.settings.mock_worker_hotkey or "5LocalMockWorkerHotkey0000000000000000000000"
    )
    worker_id = (
        f"worker-{worker_hotkey[:8]}"
        if orchestrator.settings.mock_worker_hotkey
        else "local-mock-worker"
    )

    mock_worker = Worker(
        worker_id=worker_id,
        hotkey=worker_hotkey,
        ip="127.0.0.1",
        port=9100,
        region="local",
        status=WorkerStatus.ACTIVE,
        bandwidth_mbps=1000.0,
        bandwidth_ema=1000.0,
        latency_ms=1.0,
        success_rate=1.0,
        trust_score=1.0,
        max_concurrent_tasks=100,
    )

    orchestrator.workers[mock_worker.worker_id] = mock_worker
    orchestrator.workers_by_region["local"].add(mock_worker.worker_id)

    logger.info(
        f"Added mock worker for local mode: {mock_worker.worker_id} (hotkey: {worker_hotkey})"
    )
