"""
Orchestrator Management API Routes

Endpoints for orchestrator registration and datastream creation.
These endpoints allow orchestrators to register with the subnet and manage their datastreams.

UID Allocation:
- UID 1: Subnet orchestrator (not registrable)
- UIDs 2-256: Public orchestrators (all equal, rentable via /register endpoint)
"""

import logging
from typing import List, Optional

from fastapi import APIRouter, Depends, HTTPException, Query

from core.orchestrator import Orchestrator, get_orchestrator
from routes.orchestrator_schemas import (
    DatastreamCreateRequest,
    DatastreamResponse,
    OrchestratorInfo,
    OrchestratorRegistrationRequest,
    OrchestratorRegistrationResponse,
    OrchestratorUpdateRequest,
    PUBLIC_ORCHESTRATOR_UID_END as _PUBLIC_ORCHESTRATOR_UID_END,
    PUBLIC_ORCHESTRATOR_UID_START as _PUBLIC_ORCHESTRATOR_UID_START,
    WorkerAffiliationRequest,
    WorkerAffiliationResponse,
    datastream_response,
    orchestrator_info,
)

logger = logging.getLogger(__name__)

router = APIRouter(prefix="/orchestrators", tags=["orchestrators"])

# Compatibility exports kept for callers that imported these limits from this module.
PUBLIC_ORCHESTRATOR_UID_START = _PUBLIC_ORCHESTRATOR_UID_START
PUBLIC_ORCHESTRATOR_UID_END = _PUBLIC_ORCHESTRATOR_UID_END


# =============================================================================
# Dependency
# =============================================================================


def get_orchestrator_instance() -> Orchestrator:
    """Get Orchestrator instance."""
    return get_orchestrator()


# =============================================================================
# Orchestrator Registration
# =============================================================================


@router.post("/register", response_model=OrchestratorRegistrationResponse)
async def register_orchestrator(
    request: OrchestratorRegistrationRequest,
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """
    Register a new orchestrator with the subnet.

    Requirements:
    - UID must be between 2-256 (valid orchestrator range)
    - Valid Bittensor hotkey

    """
    try:
        # Check if manager is available
        if not hasattr(orchestrator, "orch_manager") or orchestrator.orch_manager is None:
            raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

        # Check for async method (PersistentOrchestratorManager) vs sync (OrchestratorManager)
        if hasattr(orchestrator.orch_manager, "register_orchestrator_async"):
            orch = await orchestrator.orch_manager.register_orchestrator_async(
                uid=request.uid,
                hotkey=request.hotkey,
                name=request.name,
                description=request.description,
                contact=request.contact,
            )
        else:
            orch = orchestrator.orch_manager.register_orchestrator(
                uid=request.uid,
                hotkey=request.hotkey,
                name=request.name,
                description=request.description,
                contact=request.contact,
            )

        return OrchestratorRegistrationResponse(
            success=True,
            uid=orch.uid,
            hotkey=orch.hotkey,
            status=orch.status.value,
            grace_period_ends=orch.grace_period_ends,
            message=f"Orchestrator UID {orch.uid} registered successfully. "
            f"Grace period ends: {orch.grace_period_ends}",
        )

    except ValueError as e:
        return OrchestratorRegistrationResponse(
            success=False,
            message=str(e),
        )
    except Exception as e:
        logger.error(f"Orchestrator registration error: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@router.delete("/{uid}")
async def deregister_orchestrator(
    uid: int,
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """
    Deregister an orchestrator from the subnet.

    All workers affiliated with this orchestrator will be
    reassigned to Orchestrator #1 (subnet default).
    """
    if uid == 1:
        raise HTTPException(status_code=400, detail="Cannot deregister subnet orchestrator #1")

    try:
        if not hasattr(orchestrator, "orch_manager"):
            raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

        if hasattr(orchestrator.orch_manager, "deregister_orchestrator_async"):
            success = await orchestrator.orch_manager.deregister_orchestrator_async(uid)
        else:
            success = orchestrator.orch_manager.deregister_orchestrator(uid)

        if success:
            return {"success": True, "message": f"Orchestrator UID {uid} deregistered"}
        else:
            raise HTTPException(status_code=404, detail=f"Orchestrator UID {uid} not found")

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"Orchestrator deregistration error: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@router.put("/{uid}", response_model=OrchestratorInfo)
async def update_orchestrator(
    uid: int,
    request: OrchestratorUpdateRequest,
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """Update orchestrator information."""
    if not hasattr(orchestrator, "orch_manager"):
        raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

    orch = orchestrator.orch_manager.get_orchestrator(uid)
    if not orch:
        raise HTTPException(status_code=404, detail=f"Orchestrator UID {uid} not found")

    # Update fields
    if request.name is not None:
        orch.name = request.name
    if request.description is not None:
        orch.description = request.description
    if request.contact is not None:
        orch.contact = request.contact

    return orchestrator_info(orch)


# =============================================================================
# Orchestrator Queries
# =============================================================================


@router.get("/{uid}", response_model=OrchestratorInfo)
async def get_orchestrator_info(
    uid: int,
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """Get information about a specific orchestrator."""
    if not hasattr(orchestrator, "orch_manager"):
        raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

    orch = orchestrator.orch_manager.get_orchestrator(uid)
    if not orch:
        raise HTTPException(status_code=404, detail=f"Orchestrator UID {uid} not found")

    return orchestrator_info(orch)


@router.get("/", response_model=List[OrchestratorInfo])
async def list_orchestrators(
    status: Optional[str] = Query(None, description="Filter by status"),
    include_subnet: bool = Query(True, description="Include subnet orchestrator #1"),
    limit: int = Query(100, le=256, description="Max results"),
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """List all registered orchestrators."""
    if not hasattr(orchestrator, "orch_manager"):
        raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

    orchestrators = list(orchestrator.orch_manager.orchestrators.values())

    # Apply filters
    if not include_subnet:
        orchestrators = [o for o in orchestrators if not o.is_subnet_owned]

    if status:
        orchestrators = [o for o in orchestrators if o.status.value == status]

    # Sort by UID
    orchestrators.sort(key=lambda o: o.uid)

    # Apply limit
    orchestrators = orchestrators[:limit]

    return [orchestrator_info(item) for item in orchestrators]



# =============================================================================
# Datastream Management
# =============================================================================


@router.post("/{uid}/datastreams", response_model=DatastreamResponse)
async def create_datastream(
    uid: int,
    request: DatastreamCreateRequest,
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """
    Create a new datastream for an orchestrator.

    Requirements:
    - Maximum 50 datastreams per orchestrator
    - Unique datastream ID
    """
    try:
        if not hasattr(orchestrator, "orch_manager"):
            raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

        if hasattr(orchestrator.orch_manager, "create_datastream_async"):
            ds = await orchestrator.orch_manager.create_datastream_async(
                orchestrator_uid=uid,
                datastream_id=request.datastream_id,
                name=request.name,
                description=request.description,
            )
        else:
            ds = orchestrator.orch_manager.create_datastream(
                orchestrator_uid=uid,
                datastream_id=request.datastream_id,
                name=request.name,
            )

        return datastream_response(ds, uid)

    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e))
    except Exception as e:
        logger.error(f"Datastream creation error: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/{uid}/datastreams", response_model=List[DatastreamResponse])
async def list_datastreams(
    uid: int,
    include_terminated: bool = Query(False, description="Include terminated datastreams"),
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """List all datastreams for an orchestrator."""
    if not hasattr(orchestrator, "orch_manager"):
        raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

    orch = orchestrator.orch_manager.get_orchestrator(uid)
    if not orch:
        raise HTTPException(status_code=404, detail=f"Orchestrator UID {uid} not found")

    datastreams = list(orch.datastreams.values())

    if not include_terminated:
        datastreams = [d for d in datastreams if d.is_active]

    return [datastream_response(item, uid) for item in datastreams]


@router.get("/{uid}/datastreams/{datastream_id}", response_model=DatastreamResponse)
async def get_datastream(
    uid: int,
    datastream_id: str,
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """Get information about a specific datastream."""
    if not hasattr(orchestrator, "orch_manager"):
        raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

    orch = orchestrator.orch_manager.get_orchestrator(uid)
    if not orch:
        raise HTTPException(status_code=404, detail=f"Orchestrator UID {uid} not found")

    ds = orch.datastreams.get(datastream_id)
    if not ds:
        raise HTTPException(status_code=404, detail=f"Datastream {datastream_id} not found")

    return datastream_response(ds, uid)


@router.delete("/{uid}/datastreams/{datastream_id}")
async def terminate_datastream(
    uid: int,
    datastream_id: str,
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """Terminate a datastream."""
    try:
        if not hasattr(orchestrator, "orch_manager"):
            raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

        if hasattr(orchestrator.orch_manager, "terminate_datastream_async"):
            success = await orchestrator.orch_manager.terminate_datastream_async(uid, datastream_id)
        else:
            success = orchestrator.orch_manager.terminate_datastream(uid, datastream_id)

        if success:
            return {"success": True, "message": f"Datastream {datastream_id} terminated"}
        else:
            raise HTTPException(status_code=404, detail=f"Datastream {datastream_id} not found")

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"Datastream termination error: {e}")
        raise HTTPException(status_code=500, detail=str(e))


# =============================================================================
# Worker Affiliation
# =============================================================================


@router.post("/{uid}/workers", response_model=WorkerAffiliationResponse)
async def affiliate_worker(
    uid: int,
    request: WorkerAffiliationRequest,
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """
    Affiliate a worker with this orchestrator.

    Workers can voluntarily choose an orchestrator to route their traffic through.
    """
    try:
        if not hasattr(orchestrator, "orch_manager"):
            raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

        if hasattr(orchestrator.orch_manager, "affiliate_worker_async"):
            success = await orchestrator.orch_manager.affiliate_worker_async(
                worker_id=request.worker_id,
                worker_hotkey=request.worker_hotkey,
                orchestrator_uid=uid,
            )
        else:
            success = orchestrator.orch_manager.affiliate_worker(
                worker_id=request.worker_id,
                worker_hotkey=request.worker_hotkey,
                orchestrator_uid=uid,
            )

        if success:
            return WorkerAffiliationResponse(
                success=True,
                worker_id=request.worker_id,
                orchestrator_uid=uid,
                message=f"Worker affiliated with orchestrator UID {uid}",
            )
        else:
            return WorkerAffiliationResponse(
                success=False,
                worker_id=request.worker_id,
                orchestrator_uid=uid,
                message=f"Failed to affiliate worker with orchestrator UID {uid}",
            )

    except Exception as e:
        logger.error(f"Worker affiliation error: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/{uid}/workers")
async def list_affiliated_workers(
    uid: int,
    limit: int = Query(100, le=1000, description="Max results"),
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """List all workers affiliated with this orchestrator."""
    if not hasattr(orchestrator, "orch_manager"):
        raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

    orch = orchestrator.orch_manager.get_orchestrator(uid)
    if not orch:
        raise HTTPException(status_code=404, detail=f"Orchestrator UID {uid} not found")

    workers = list(orch.worker_hotkeys)[:limit]

    return {
        "orchestrator_uid": uid,
        "total_workers": len(orch.worker_hotkeys),
        "workers": [{"hotkey": hk[:16] + "..."} for hk in workers],
    }


# =============================================================================
# Statistics
# =============================================================================


@router.get("/stats/summary")
async def get_orchestrator_stats(
    orchestrator: Orchestrator = Depends(get_orchestrator_instance),
):
    """Get aggregate orchestrator statistics."""
    if not hasattr(orchestrator, "orch_manager"):
        raise HTTPException(status_code=503, detail="Orchestrator manager not initialized")

    orchestrators = list(orchestrator.orch_manager.orchestrators.values())
    non_subnet = [o for o in orchestrators if not o.is_subnet_owned]

    # Status distribution
    status_counts = {}
    for o in orchestrators:
        status = o.status.value
        status_counts[status] = status_counts.get(status, 0) + 1

    # Total workers
    total_workers = sum(len(o.worker_hotkeys) for o in orchestrators)

    # Total datastreams
    total_datastreams = sum(
        len([d for d in o.datastreams.values() if d.is_active]) for o in orchestrators
    )


    return {
        "total_orchestrators": len(orchestrators),
        "non_subnet_orchestrators": len(non_subnet),
        "orchestrators_by_status": status_counts,
        "total_workers": total_workers,
        "total_datastreams": total_datastreams,
        "subnet_orchestrator": (
            {
                "uid": 1,
                "workers": len(orchestrators[0].worker_hotkeys) if orchestrators else 0,
            }
            if orchestrators and orchestrators[0].is_subnet_owned
            else None
        ),
    }
