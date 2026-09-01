"""Worker lifecycle orchestration, independent from transport implementations."""

import asyncio
import signal

try:
    from .models import WorkerState
except ImportError:  # Direct execution through worker.py
    from models import WorkerState


async def run_worker(
    state: WorkerState,
    *,
    httpx_module,
    register_worker_fn,
    websocket_loop_fn,
    task_executor_fn,
    connection_mode: str,
    websockets_available: bool,
) -> None:
    """Register the worker, run its transport, then release owned resources."""
    hotkey = state.wallet.hotkey.ss58_address
    state.http_client = httpx_module.AsyncClient(
        timeout=httpx_module.Timeout(connect=10.0, read=60.0, write=60.0, pool=5.0),
    )
    state.ws_executor_task = asyncio.create_task(task_executor_fn(state))

    try:
        async with httpx_module.AsyncClient() as client:
            print("[Worker] Registering with SubnetCore...")
            print(f"[Worker] Hotkey: {hotkey}")
            print(f"[Worker] API URL: {state.api_url}")
            result = await register_worker_fn(client, state)
            state.worker_id = result.get("worker_id")
            state.api_key = result.get("api_key")
            print(f"[Worker] Registered: {state.worker_id}")

        _validate_transport(state, connection_mode, websockets_available)
        print("[Worker] Starting WebSocket connection (worker gateway transport)")
        await websocket_loop_fn(state)
    except asyncio.CancelledError:
        print("[Worker] Cancelled")
    except Exception as exc:
        print(f"[Worker] Error: {exc}")
        raise
    finally:
        await _close_worker_resources(state)

    print("[Worker] Stopped")


def _validate_transport(
    state: WorkerState,
    connection_mode: str,
    websockets_available: bool,
) -> None:
    if connection_mode not in {"websocket", "auto"}:
        raise RuntimeError("Worker transport is websocket-only; remove CONNECTION_MODE=http")
    if not websockets_available:
        raise RuntimeError("websockets library is required for worker gateway transport")
    if not state.worker_gateway_url:
        raise RuntimeError("WORKER_GATEWAY_URL must point to an orchestrator-owned worker gateway")


async def _close_worker_resources(state: WorkerState) -> None:
    if state.ws_executor_task is not None:
        pending_count = state.ws_offer_queue.qsize()
        if pending_count:
            print(f"[Worker] Waiting for {pending_count} queued task(s) to finish sequentially")
        await state.ws_offer_queue.put(None)
        await state.ws_executor_task
        state.ws_executor_task = None
    if state.http_client:
        await state.http_client.aclose()
        state.http_client = None


async def main(
    *,
    bt_module,
    environ,
    get_config_fn,
    apply_config_overrides_fn,
    run_worker_fn,
    shutdown_signal: asyncio.Event,
    mainnet_url: str,
    testnet_url: str,
    exit_fn,
) -> None:
    """Create runtime state, install signals, and run the worker."""
    print("Beam Network Worker")
    print("=" * 40)
    config = get_config_fn()
    apply_config_overrides_fn(config)
    wallet = bt_module.Wallet(config=config)
    print(f"Wallet name: {wallet.name}")
    print(f"Hotkey name: {wallet.hotkey_str}")

    try:
        _ = wallet.hotkey
        print(f"Hotkey address: {wallet.hotkey.ss58_address}")
    except Exception as exc:
        print(f"Failed to load hotkey: {exc}")
        exit_fn(1)

    api_url = _resolve_api_url(config, environ, mainnet_url, testnet_url)
    gateway_url = environ.get("WORKER_GATEWAY_URL")
    _print_runtime_config(api_url, gateway_url)
    state = WorkerState(wallet=wallet, api_url=api_url, worker_gateway_url=gateway_url)
    loop = asyncio.get_running_loop()

    def handle_shutdown():
        print("\nShutting down worker...")
        state.running = False
        shutdown_signal.set()

    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, handle_shutdown)
    try:
        await run_worker_fn(state)
    finally:
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.remove_signal_handler(sig)
    print("Worker stopped")


def _resolve_api_url(config, environ, mainnet_url: str, testnet_url: str) -> str:
    network = config.subtensor.get("network", "finney")
    if network in ("test", "testnet"):
        print("Network: testnet")
        api_url = environ.get("CORE_SERVER_URL") or testnet_url
        if not api_url:
            raise RuntimeError("CORE_SERVER_URL is required when running against testnet")
        return api_url
    print("Network: mainnet")
    return environ.get("CORE_SERVER_URL", mainnet_url)


def _print_runtime_config(api_url: str, gateway_url) -> None:
    print(f"API URL: {api_url}")
    if gateway_url:
        print(f"Worker gateway URL: {gateway_url}")
    else:
        print("Worker gateway URL: MISSING")
    print("Worker execution: sequential FIFO (one task at a time), offer_queue=unbounded")
    print()
