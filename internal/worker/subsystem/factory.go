package subsystem

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
	actionhandler "github.com/Beam-Network/beam/internal/workload/handlers/action"
	distributionhandler "github.com/Beam-Network/beam/internal/workload/handlers/distribution"
	roomhandler "github.com/Beam-Network/beam/internal/workload/handlers/room"
	roommediahandler "github.com/Beam-Network/beam/internal/workload/handlers/roommedia"
	roomtransferhandler "github.com/Beam-Network/beam/internal/workload/handlers/roomtransfer"
	roomworkloadhandlers "github.com/Beam-Network/beam/internal/workload/handlers/roomworkloads"
	"github.com/Beam-Network/beam/internal/workload/handlers/transfer"
	tunnelhandler "github.com/Beam-Network/beam/internal/workload/handlers/tunnel"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

type Config struct {
	ActionRoot                string        `json:"action_root,omitempty"`
	ActionCache               string        `json:"action_cache,omitempty"`
	ActionScratch             string        `json:"action_scratch,omitempty"`
	ActionNodeBinary          string        `json:"action_node_binary,omitempty"`
	ActionOCIBinary           string        `json:"action_oci_binary,omitempty"`
	ActionWasmtime            string        `json:"action_wasmtime,omitempty"`
	ActionPermissions         []string      `json:"action_permissions,omitempty"`
	ActionHostRPCMethods      []string      `json:"action_host_rpc_methods,omitempty"`
	ActionRegistryHosts       []string      `json:"action_registry_hosts,omitempty"`
	ActionControlHosts        []string      `json:"action_control_hosts,omitempty"`
	ActionPublisherKeys       []string      `json:"action_publisher_keys,omitempty"`
	ActionMaximumTimeout      time.Duration `json:"action_maximum_timeout,omitempty"`
	ActionMemoryLimitMB       int64         `json:"action_memory_limit_mb,omitempty"`
	ActionOutputLimitBytes    int64         `json:"action_output_limit_bytes,omitempty"`
	ActionMaximumBytes        int64         `json:"action_maximum_bytes,omitempty"`
	RequirePublisherSignature bool          `json:"require_publisher_signature"`
	AllowLegacyNode           bool          `json:"allow_legacy_node"`
	AllowPublicListeners      bool          `json:"allow_public_listeners"`
	ControlAddress            string        `json:"control_address,omitempty"`
	ControlToken              string        `json:"control_token,omitempty"`
	MediaListenAddress        string        `json:"media_listen_address,omitempty"`
	MediaAdvertiseURL         string        `json:"media_advertise_url,omitempty"`
	MediaPublicIP             string        `json:"media_public_ip,omitempty"`
	MediaUDPPortMin           uint16        `json:"media_udp_port_min,omitempty"`
	MediaUDPPortMax           uint16        `json:"media_udp_port_max,omitempty"`
	MediaICEServers           []string      `json:"media_ice_servers,omitempty"`
	MediaTURNSecret           string        `json:"media_turn_secret,omitempty"`
	MediaTURNCredentialTTL    time.Duration `json:"media_turn_credential_ttl,omitempty"`
	MediaTURNUsernamePrefix   string        `json:"media_turn_username_prefix,omitempty"`
	MediaMaxViewers           int           `json:"media_max_viewers,omitempty"`
	MediaWebRTCEnabled        bool          `json:"media_webrtc_enabled,omitempty"`
}

func (c Config) ForKind(kind domain.Kind) Config {
	switch kind {
	case domain.KindTransferDistribute:
		return Config{ControlAddress: c.ControlAddress, ControlToken: c.ControlToken}
	case domain.KindTransferMultipart, domain.KindRoomTransfer, domain.KindRoomDatagram,
		domain.KindRoomMessage, domain.KindRoomCommand, domain.KindRoomStream:
		return Config{}
	case domain.KindActionExecute:
		return Config{
			ActionRoot: c.ActionRoot, ActionCache: c.ActionCache, ActionScratch: c.ActionScratch,
			ActionNodeBinary: c.ActionNodeBinary, ActionOCIBinary: c.ActionOCIBinary, ActionWasmtime: c.ActionWasmtime,
			ActionPermissions: c.ActionPermissions, ActionHostRPCMethods: c.ActionHostRPCMethods,
			ActionRegistryHosts: c.ActionRegistryHosts, ActionControlHosts: c.ActionControlHosts,
			ActionPublisherKeys: c.ActionPublisherKeys, ActionMaximumTimeout: c.ActionMaximumTimeout,
			ActionMemoryLimitMB: c.ActionMemoryLimitMB, ActionOutputLimitBytes: c.ActionOutputLimitBytes,
			ActionMaximumBytes: c.ActionMaximumBytes, RequirePublisherSignature: c.RequirePublisherSignature,
			AllowLegacyNode: c.AllowLegacyNode,
		}
	case domain.KindTunnelHTTP, domain.KindTunnelTCP:
		return Config{AllowPublicListeners: c.AllowPublicListeners}
	case domain.KindRoomMedia:
		return Config{MediaListenAddress: c.MediaListenAddress, MediaAdvertiseURL: c.MediaAdvertiseURL,
			MediaPublicIP: c.MediaPublicIP, MediaUDPPortMin: c.MediaUDPPortMin, MediaUDPPortMax: c.MediaUDPPortMax,
			MediaICEServers: append([]string(nil), c.MediaICEServers...), MediaTURNSecret: c.MediaTURNSecret,
			MediaTURNCredentialTTL: c.MediaTURNCredentialTTL, MediaTURNUsernamePrefix: c.MediaTURNUsernamePrefix,
			MediaMaxViewers:    c.MediaMaxViewers,
			MediaWebRTCEnabled: c.MediaWebRTCEnabled}
	default:
		return Config{}
	}
}

func ConfigFromEnvironment() (Config, error) {
	encoded := os.Getenv(ConfigEnvironment)
	if encoded == "" {
		return Config{}, errors.New("subsystem configuration is missing")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Config{}, errors.New("subsystem configuration is not valid base64url")
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func BuildHandler(kind domain.Kind, config Config) (runtime.Handler, error) {
	switch kind {
	case domain.KindTransferMultipart:
		return transfer.NewHandler(nil), nil
	case domain.KindTransferDistribute:
		transport, err := newCircuitTransport(config.ControlAddress, config.ControlToken)
		if err != nil {
			return nil, err
		}
		return distributionhandler.NewHandler(func() distributionhandler.Transport { return transport }, nil), nil
	case domain.KindActionExecute:
		return actionhandler.NewHandler(actionhandler.Config{
			ActionRoot: config.ActionRoot, CacheRoot: config.ActionCache, ScratchRoot: config.ActionScratch,
			NodeBinary: config.ActionNodeBinary, OCIBinary: config.ActionOCIBinary, WasmtimeBinary: config.ActionWasmtime,
			AllowedPermissions: config.ActionPermissions, AllowedHostRPCMethods: config.ActionHostRPCMethods,
			RegistryHosts: config.ActionRegistryHosts, ControlHosts: config.ActionControlHosts,
			TrustedPublisherKeys: config.ActionPublisherKeys, MaximumTimeout: config.ActionMaximumTimeout,
			MemoryLimitMB: config.ActionMemoryLimitMB, OutputLimitBytes: config.ActionOutputLimitBytes,
			MaximumActionBytes: config.ActionMaximumBytes, RequirePublisherSignature: config.RequirePublisherSignature,
			AllowLegacyNode: config.AllowLegacyNode,
		})
	case domain.KindTunnelHTTP:
		return tunnelhandler.NewHTTPHandler(tunnelhandler.Config{AllowPublicListeners: config.AllowPublicListeners}), nil
	case domain.KindTunnelTCP:
		return tunnelhandler.NewTCPHandler(tunnelhandler.Config{AllowPublicListeners: config.AllowPublicListeners}), nil
	case domain.KindRoomMedia:
		if config.MediaWebRTCEnabled {
			return roommediahandler.NewHandler(roommediahandler.Config{ListenAddress: config.MediaListenAddress,
				AdvertiseURL: config.MediaAdvertiseURL, PublicIP: config.MediaPublicIP, UDPPortMin: config.MediaUDPPortMin,
				UDPPortMax: config.MediaUDPPortMax, ICEServers: config.MediaICEServers, TURNSecret: config.MediaTURNSecret,
				TURNCredentialTTL: config.MediaTURNCredentialTTL, TURNUsernamePrefix: config.MediaTURNUsernamePrefix,
				MaxViewers: config.MediaMaxViewers}), nil
		}
		return roomhandler.NewHandler(roomhandler.Config{AllowPublicListeners: config.AllowPublicListeners}), nil
	case domain.KindRoomDatagram:
		return roomworkloadhandlers.NewDatagramHandler(nil), nil
	case domain.KindRoomMessage:
		return roomworkloadhandlers.NewMessageHandler(nil), nil
	case domain.KindRoomCommand:
		return roomworkloadhandlers.NewCommandHandler(nil), nil
	case domain.KindRoomStream:
		return roomworkloadhandlers.NewStreamHandler(nil), nil
	case domain.KindRoomTransfer:
		return roomtransferhandler.NewHandler(nil), nil
	default:
		return nil, errors.New("workload kind has no isolated subsystem implementation")
	}
}
