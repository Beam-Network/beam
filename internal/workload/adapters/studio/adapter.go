package studio

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type Task struct {
	TaskID              string                             `json:"task_id"`
	AttemptID           string                             `json:"attempt_id"`
	ActionPackageName   string                             `json:"action_package_name"`
	ActionVersion       string                             `json:"action_version,omitempty"`
	Entrypoint          string                             `json:"entrypoint"`
	ArtifactSHA256      string                             `json:"artifact_sha256"`
	TimeoutSeconds      int64                              `json:"timeout_seconds,omitempty"`
	Input               map[string]any                     `json:"input,omitempty"`
	Config              map[string]any                     `json:"config,omitempty"`
	RequiredPermissions []string                           `json:"required_permissions,omitempty"`
	RegistryArtifact    *contracts.ActionRegistryArtifact  `json:"registry_artifact,omitempty"`
	SecretBroker        *contracts.ActionSecretBroker      `json:"secret_broker,omitempty"`
	ArtifactPublisher   *contracts.ActionArtifactPublisher `json:"artifact_publisher,omitempty"`
	Sandbox             contracts.ActionSandbox            `json:"sandbox"`
	HostRPCMethods      []string                           `json:"host_rpc_methods,omitempty"`
	LeaseExpiresAt      time.Time                          `json:"lease_expires_at"`
}

func ToWorkload(task Task, identity domain.Identity, now time.Time) (domain.Spec, error) {
	if task.TaskID == "" || task.AttemptID == "" || task.ActionPackageName == "" ||
		task.Entrypoint == "" || task.ArtifactSHA256 == "" {
		return domain.Spec{}, errors.New("Studio task_id, attempt_id, action package, entrypoint, and artifact SHA-256 are required")
	}
	if task.Sandbox.Runtime != "oci" && task.Sandbox.Runtime != "wasi" && task.Sandbox.Runtime != "node-legacy" {
		return domain.Spec{}, errors.New("Studio task requires an explicit OCI, WASI, or legacy Node sandbox runtime")
	}
	if (task.Sandbox.Runtime == "oci" || task.Sandbox.Runtime == "wasi") && task.RegistryArtifact == nil {
		return domain.Spec{}, errors.New("strong Studio sandboxes require a registry artifact capability")
	}
	hostMethods := task.HostRPCMethods
	if len(hostMethods) == 0 && task.Sandbox.Runtime != "node-legacy" {
		hostMethods = defaultHostRPCMethods(task)
	}
	payload, err := json.Marshal(contracts.ActionExecution{
		TaskID: task.TaskID, ActionPackageName: task.ActionPackageName,
		ActionVersion: task.ActionVersion, Entrypoint: task.Entrypoint,
		ArtifactSHA256: task.ArtifactSHA256, TimeoutSeconds: task.TimeoutSeconds,
		Input: task.Input, Config: task.Config, RegistryArtifact: task.RegistryArtifact,
		SecretBroker: task.SecretBroker, ArtifactPublisher: task.ArtifactPublisher,
		Sandbox: task.Sandbox, HostRPCMethods: hostMethods,
	})
	if err != nil {
		return domain.Spec{}, err
	}
	return domain.Spec{
		WorkloadID: task.TaskID, AttemptID: task.AttemptID, Identity: identity,
		Kind: domain.KindActionExecute, Class: domain.ClassJob,
		Source:               domain.Source{System: "beam-studio", Reference: task.TaskID},
		RequiredCapabilities: []string{"action.execute"},
		Resources:            domain.Resources{CPUMillis: 1000, MemoryBytes: 128 << 20, ScratchBytes: 64 << 20},
		Lease:                domain.Lease{OfferExpiresAt: minTime(task.LeaseExpiresAt, now.Add(30*time.Second))},
		Security:             domain.SecurityPolicy{Permissions: task.RequiredPermissions, TrustProfile: "studio-action"},
		Evidence:             domain.EvidencePolicy{ReceiptRequired: true}, Payload: payload,
	}, nil
}

func defaultHostRPCMethods(task Task) []string {
	methods := []string{"logger.debug", "logger.info", "logger.warn", "logger.error", "state.get", "state.set", "state.patch"}
	for _, permission := range task.RequiredPermissions {
		if permission == "storage:write" || permission == "storage:*" {
			if task.ArtifactPublisher != nil {
				methods = append(methods, "artifacts.publish")
			}
		}
		if permission == "secrets:read" || permission == "secrets:*" {
			if task.SecretBroker != nil {
				methods = append(methods, "secrets.get")
			}
		}
	}
	return methods
}

func minTime(left, right time.Time) time.Time {
	if left.IsZero() || right.Before(left) {
		return right
	}
	return left
}
