"""Public schemas and response mappers for orchestrator management routes."""

from datetime import datetime
from typing import Optional

from pydantic import BaseModel, Field


PUBLIC_ORCHESTRATOR_UID_START = 2
PUBLIC_ORCHESTRATOR_UID_END = 256


class OrchestratorRegistrationRequest(BaseModel):
    """Request to register a new orchestrator (UIDs 2-256)."""

    uid: int = Field(
        ...,
        ge=PUBLIC_ORCHESTRATOR_UID_START,
        le=PUBLIC_ORCHESTRATOR_UID_END,
        description=f"Orchestrator UID ({PUBLIC_ORCHESTRATOR_UID_START}-{PUBLIC_ORCHESTRATOR_UID_END})",
    )
    hotkey: str = Field(..., min_length=48, description="Bittensor hotkey")
    name: Optional[str] = Field(None, max_length=100, description="Orchestrator name")
    description: Optional[str] = Field(None, max_length=500, description="Description")
    contact: Optional[str] = Field(None, max_length=100, description="Contact info (email/discord)")


class OrchestratorRegistrationResponse(BaseModel):
    """Response from orchestrator registration."""

    success: bool
    uid: Optional[int] = None
    hotkey: Optional[str] = None
    status: Optional[str] = None
    grace_period_ends: Optional[datetime] = None
    message: str


class OrchestratorUpdateRequest(BaseModel):
    """Request to update orchestrator info."""

    name: Optional[str] = Field(None, max_length=100)
    description: Optional[str] = Field(None, max_length=500)
    contact: Optional[str] = Field(None, max_length=100)


class DatastreamCreateRequest(BaseModel):
    """Request to create a new datastream."""

    datastream_id: str = Field(..., min_length=1, max_length=64, description="Unique datastream ID")
    name: str = Field(..., min_length=1, max_length=100, description="Datastream name")
    description: Optional[str] = Field(None, max_length=500, description="Description")


class DatastreamResponse(BaseModel):
    """Datastream information."""

    datastream_id: str
    orchestrator_uid: int
    name: str
    status: str
    created_at: datetime
    total_bytes_transferred: int = 0
    total_transfers: int = 0
    success_rate: float = 0.0


class OrchestratorInfo(BaseModel):
    """Orchestrator information response."""

    uid: int
    hotkey: str
    status: str
    is_subnet_owned: bool
    name: Optional[str] = None
    description: Optional[str] = None
    contact: Optional[str] = None
    registered_at: datetime
    grace_period_ends: Optional[datetime] = None
    worker_count: int = 0
    datastream_count: int = 0


class WorkerAffiliationRequest(BaseModel):
    """Request to affiliate a worker with an orchestrator."""

    worker_id: str = Field(..., description="Worker ID")
    worker_hotkey: str = Field(..., description="Worker's hotkey")


class WorkerAffiliationResponse(BaseModel):
    """Response from worker affiliation."""

    success: bool
    worker_id: str
    orchestrator_uid: int
    message: str


def orchestrator_info(value) -> OrchestratorInfo:
    return OrchestratorInfo(
        uid=value.uid,
        hotkey=value.hotkey,
        status=value.status.value,
        is_subnet_owned=value.is_subnet_owned,
        name=value.name,
        description=value.description,
        contact=value.contact,
        registered_at=value.registered_at,
        grace_period_ends=value.grace_period_ends,
        worker_count=len(value.worker_hotkeys),
        datastream_count=len([item for item in value.datastreams.values() if item.is_active]),
    )


def datastream_response(value, orchestrator_uid: int) -> DatastreamResponse:
    return DatastreamResponse(
        datastream_id=value.datastream_id,
        orchestrator_uid=orchestrator_uid,
        name=value.name,
        status=value.status.value,
        created_at=value.created_at,
        total_bytes_transferred=value.total_bytes_transferred,
        total_transfers=value.total_transfers,
        success_rate=value.success_rate,
    )
