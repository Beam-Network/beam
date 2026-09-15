package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/beamlink/wcp"
	"github.com/Beam-Network/beam/internal/evidence"
	"github.com/Beam-Network/beam/internal/resources"
	versioninfo "github.com/Beam-Network/beam/internal/version"
	"github.com/Beam-Network/beam/internal/worker/server"
	"github.com/Beam-Network/beam/internal/worker/subsystem"
	"github.com/Beam-Network/beam/internal/workload/contracts"
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

var version = versioninfo.Version

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "serve":
		serve(os.Args[2:])
	case "subsystem":
		serveSubsystem(os.Args[2:])
	case "doctor":
		doctor(os.Args[2:])
	case "node-id":
		printNodeID(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func serve(arguments []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	workerID := flags.String("worker-id", os.Getenv("BEAM_WORKER_ID"), "canonical BeamCore worker_id")
	orchestratorID := flags.String("orchestrator-id", os.Getenv("BEAM_ORCHESTRATOR_ID"), "Orchestrator membership id")
	nodeID := flags.String("node-id", os.Getenv("BEAM_NODE_ID"), "BeamLink node identity")
	addr := flags.String("addr", "127.0.0.1:8780", "owner-local control listen address")
	token := flags.String("control-token", os.Getenv("BEAM_WORKER_CONTROL_TOKEN"), "optional bearer token for the local control API")
	memory := flags.Int64("memory-bytes", 512<<20, "reservable memory")
	scratch := flags.Int64("scratch-bytes", 10<<30, "reservable scratch bytes")
	bandwidth := flags.Int64("bandwidth-mbps", 100, "reservable bandwidth")
	capabilityList := flags.String("capabilities", "transfer.multipart,transfer.multipart.fanout.v1,room.transfer,room.transfer.direct.v1,room.transfer.e2ee.v2", "comma-separated enabled capabilities")
	statePath := flags.String("state", "data/worker/workloads.json", "durable workload journal")
	actionRoot := flags.String("action-root", os.Getenv("BEAM_ACTION_ROOT"), "root of checksum-pinned Studio actions")
	actionCache := flags.String("action-cache", "data/worker/action-cache", "content-addressed Studio action cache")
	actionScratch := flags.String("action-scratch", "data/worker/action-scratch", "Studio sandbox scratch root")
	actionPermissions := flags.String("action-permissions", "storage:read,storage:write", "permissions allowed for Studio actions")
	actionRegistryHosts := flags.String("action-registry-hosts", os.Getenv("BEAM_ACTION_REGISTRY_HOSTS"), "comma-separated action registry and object-storage hosts")
	actionControlHosts := flags.String("action-control-hosts", os.Getenv("BEAM_ACTION_CONTROL_HOSTS"), "comma-separated secret broker and artifact publisher hosts")
	actionOCIBinary := flags.String("action-oci-runtime", os.Getenv("BEAM_ACTION_OCI_RUNTIME"), "Docker or Podman binary for OCI actions")
	actionWasmtime := flags.String("action-wasmtime", os.Getenv("BEAM_ACTION_WASMTIME"), "Wasmtime binary for WASI actions")
	actionPublisherSignature := flags.Bool("action-require-publisher-signature", true, "require Ed25519 signatures on downloaded action artifacts")
	actionPublisherKeys := flags.String("action-publisher-keys", os.Getenv("BEAM_ACTION_PUBLISHER_KEYS"), "comma-separated trusted base64url Ed25519 action publisher keys")
	actionLegacyNode := flags.Bool("action-allow-legacy-node", false, "allow the weaker legacy Node subprocess sandbox")
	allowPublicNetwork := flags.Bool("allow-public-network-listeners", false, "allow assignment-token-protected public tunnel and room listeners")
	mediaAddr := flags.String("media-addr", envOrDefault("BEAM_MEDIA_LISTEN_ADDR", "127.0.0.1:0"), "WHIP/WHEP signaling listen address for worker-hosted room media")
	mediaAdvertiseURL := flags.String("media-advertise-url", os.Getenv("BEAM_MEDIA_ADVERTISE_URL"), "public base URL reaching the worker media listener")
	roomTransferAddr := flags.String("room-transfer-addr", envOrDefault("BEAM_ROOM_TRANSFER_LISTEN_ADDR", "127.0.0.1:0"), "HTTP listen address for Worker-hosted room transfers")
	roomTransferAdvertiseURL := flags.String("room-transfer-advertise-url", os.Getenv("BEAM_ROOM_TRANSFER_ADVERTISE_URL"), "public base URL reaching the Worker room-transfer listener")
	roomStorageAddr := flags.String("room-storage-addr", os.Getenv("BEAM_ROOM_STORAGE_LISTEN_ADDR"), "TLS listen address for worker-executed hybrid room transfers")
	roomStorageAdvertiseURL := flags.String("room-storage-advertise-url", os.Getenv("BEAM_ROOM_STORAGE_ADVERTISE_URL"), "HTTPS URL reaching the hybrid room listener")
	mediaPublicIP := flags.String("media-public-ip", os.Getenv("BEAM_MEDIA_PUBLIC_IP"), "public IP announced in worker WebRTC ICE candidates")
	mediaUDPPortMin := flags.Uint("media-udp-port-min", uint(envIntOrDefault("BEAM_MEDIA_UDP_PORT_MIN", 0)), "minimum worker WebRTC UDP port")
	mediaUDPPortMax := flags.Uint("media-udp-port-max", uint(envIntOrDefault("BEAM_MEDIA_UDP_PORT_MAX", 0)), "maximum worker WebRTC UDP port")
	mediaICEServers := flags.String("media-ice-servers", os.Getenv("BEAM_MEDIA_ICE_SERVERS"), "comma-separated STUN/TURN URLs offered by the worker SFU")
	mediaTURNSecret := flags.String("media-turn-secret", os.Getenv("BEAM_MEDIA_TURN_SECRET"), "coturn REST shared secret used for temporary room media credentials")
	mediaTURNCredentialTTL := flags.Duration("media-turn-credential-ttl", envDurationOrDefault("BEAM_MEDIA_TURN_CREDENTIAL_TTL", 3*time.Hour), "lifetime of temporary room media TURN credentials")
	mediaTURNUsernamePrefix := flags.String("media-turn-username-prefix", envOrDefault("BEAM_MEDIA_TURN_USERNAME_PREFIX", "beam"), "prefix for temporary room media TURN usernames")
	mediaMaxViewers := flags.Int("media-max-viewers", envIntOrDefault("BEAM_MEDIA_MAX_VIEWERS", 64), "maximum WHEP viewers per worker media session")
	mediaPublisherReconnectGrace := flags.Duration("media-publisher-reconnect-grace", envDurationOrDefault("BEAM_MEDIA_PUBLISHER_RECONNECT_GRACE", 30*time.Second), "time a room media flow remains active while its publisher reconnects")
	wcpAddress := flags.String("wcp-address", os.Getenv("BEAM_WCP_ADDRESS"), "Orchestrator WCP host:port; empty disables WCP")
	wcpCA := flags.String("wcp-ca", os.Getenv("BEAM_WCP_CA"), "Orchestrator WCP certificate authority")
	wcpServerName := flags.String("wcp-server-name", os.Getenv("BEAM_WCP_SERVER_NAME"), "Orchestrator TLS server name")
	nodeKeyPath := flags.String("node-key", "data/worker/node.key", "Ed25519 WCP node key")
	circuitAddress := flags.String("circuit-addr", os.Getenv("BEAM_CIRCUIT_LISTEN_ADDR"), "direct Worker-to-Worker TLS listen address; empty disables Circuits")
	circuitAdvertise := flags.String("circuit-advertise", os.Getenv("BEAM_CIRCUIT_ADVERTISE"), "reachable host:port announced to the Orchestrator; defaults to the bound address")
	circuitState := flags.String("circuit-state", "data/worker/circuits.json", "durable data-plane circuit table")
	receiptState := flags.String("receipt-state", "data/worker/receipts.json", "durable signed receipt journal")
	subsystemMode := flags.String("subsystem-mode", envOrDefault("BEAM_WORKER_SUBSYSTEM_MODE", "process"), "handler isolation mode: process or integrated")
	region := flags.String("region", os.Getenv("BEAM_REGION"), "Worker region")
	delegation := flags.String("orchestrator-delegation", os.Getenv("BEAM_ORCHESTRATOR_DELEGATION"), "base64url Orchestrator delegation")
	_ = flags.Parse(arguments)
	if *mediaUDPPortMin > 65535 || *mediaUDPPortMax > 65535 {
		log.Fatal("media UDP ports must be between 1 and 65535")
	}

	var nodePrivateKey ed25519.PrivateKey
	if *wcpAddress != "" || *circuitAddress != "" {
		var err error
		nodePrivateKey, err = wcp.LoadOrCreateNodeKey(*nodeKeyPath)
		if err != nil {
			log.Fatal(err)
		}
		derivedNodeID := wcp.NodeID(nodePrivateKey.Public().(ed25519.PublicKey))
		if *nodeID == "" {
			*nodeID = derivedNodeID
		}
	}
	identity := domain.Identity{WorkerID: *workerID, OrchestratorID: *orchestratorID, NodeID: *nodeID, InstanceID: fmt.Sprintf("instance_%d", time.Now().UnixNano())}
	if err := identity.Validate(); err != nil {
		log.Fatal(err)
	}
	controlListener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	controlAddress := controlListener.Addr().String()
	capabilities := splitList(*capabilityList)
	var circuitService *circuit.Service
	governor, err := resources.NewGovernor(domain.Resources{
		CPUMillis: 1000, MemoryBytes: *memory, ScratchBytes: *scratch,
		BandwidthMbps: *bandwidth, Connections: 1024, Streams: 1024,
	})
	if err != nil {
		log.Fatal(err)
	}
	registry := runtime.NewRegistry()
	processConfig := subsystem.Config{
		ActionRoot: *actionRoot, ActionCache: *actionCache, ActionScratch: *actionScratch,
		ActionOCIBinary: *actionOCIBinary, ActionWasmtime: *actionWasmtime,
		ActionPermissions: splitList(*actionPermissions), ActionRegistryHosts: splitList(*actionRegistryHosts),
		ActionControlHosts: splitList(*actionControlHosts), ActionPublisherKeys: splitList(*actionPublisherKeys),
		RequirePublisherSignature: *actionPublisherSignature, AllowLegacyNode: *actionLegacyNode,
		AllowPublicListeners: *allowPublicNetwork, ControlAddress: controlAddress, ControlToken: *token,
		MediaListenAddress: *mediaAddr, MediaAdvertiseURL: *mediaAdvertiseURL, MediaPublicIP: *mediaPublicIP,
		MediaUDPPortMin: uint16(*mediaUDPPortMin), MediaUDPPortMax: uint16(*mediaUDPPortMax),
		MediaICEServers: splitList(*mediaICEServers), MediaTURNSecret: *mediaTURNSecret,
		MediaTURNCredentialTTL: *mediaTURNCredentialTTL, MediaTURNUsernamePrefix: *mediaTURNUsernamePrefix,
		MediaMaxViewers:    *mediaMaxViewers,
		MediaWebRTCEnabled: contains(capabilities, contracts.RoomMediaWebRTCCapability),
	}
	if *subsystemMode != "process" && *subsystemMode != "integrated" {
		log.Fatal("--subsystem-mode must be process or integrated")
	}
	registerHandler := func(handler runtime.Handler) {
		if *subsystemMode == "process" {
			isolated, isolateErr := subsystem.NewProcessHandler(handler, processConfig)
			if isolateErr != nil {
				log.Fatal(isolateErr)
			}
			handler = isolated
		}
		if registerErr := registry.Register(handler); registerErr != nil {
			log.Fatal(registerErr)
		}
	}
	if contains(capabilities, "transfer.multipart") {
		registerHandler(transfer.NewHandler(nil))
	}
	if contains(capabilities, "transfer.distribute") {
		registerHandler(distributionhandler.NewHandler(func() distributionhandler.Transport {
			return circuitService
		}, nil))
	}
	if contains(capabilities, "action.execute") {
		handler, err := actionhandler.NewHandler(actionhandler.Config{
			ActionRoot: *actionRoot, CacheRoot: *actionCache, ScratchRoot: *actionScratch,
			AllowedPermissions: splitList(*actionPermissions), RegistryHosts: splitList(*actionRegistryHosts),
			ControlHosts: splitList(*actionControlHosts), OCIBinary: *actionOCIBinary, WasmtimeBinary: *actionWasmtime,
			RequirePublisherSignature: *actionPublisherSignature, TrustedPublisherKeys: splitList(*actionPublisherKeys),
			AllowLegacyNode: *actionLegacyNode,
		})
		if err != nil {
			log.Fatal(err)
		}
		registerHandler(handler)
	}
	if contains(capabilities, "tunnel.http") {
		registerHandler(tunnelhandler.NewHTTPHandler(tunnelhandler.Config{AllowPublicListeners: *allowPublicNetwork}))
	}
	if contains(capabilities, "tunnel.tcp") {
		registerHandler(tunnelhandler.NewTCPHandler(tunnelhandler.Config{AllowPublicListeners: *allowPublicNetwork}))
	}
	if contains(capabilities, contracts.RoomMediaWebRTCCapability) {
		mediaHandler := roommediahandler.NewHandler(roommediahandler.Config{ListenAddress: *mediaAddr,
			AdvertiseURL: *mediaAdvertiseURL, PublicIP: *mediaPublicIP, UDPPortMin: uint16(*mediaUDPPortMin),
			UDPPortMax: uint16(*mediaUDPPortMax), ICEServers: splitList(*mediaICEServers), TURNSecret: *mediaTURNSecret,
			TURNCredentialTTL: *mediaTURNCredentialTTL, TURNUsernamePrefix: *mediaTURNUsernamePrefix,
			MaxViewers: *mediaMaxViewers, PublisherReconnectGrace: *mediaPublisherReconnectGrace})
		// WebRTC sessions share one signaling listener and are multiplexed by
		// session ID. Keep this stateful handler in the Worker process even when
		// other workload kinds use subprocess isolation.
		if registerErr := registry.Register(mediaHandler); registerErr != nil {
			log.Fatal(registerErr)
		}
	} else if contains(capabilities, "room.media") {
		registerHandler(roomhandler.NewHandler(roomhandler.Config{AllowPublicListeners: *allowPublicNetwork}))
	}
	if contains(capabilities, "room.datagram") {
		registerHandler(roomworkloadhandlers.NewDatagramHandler(nil))
	}
	if contains(capabilities, "room.message") {
		registerHandler(roomworkloadhandlers.NewMessageHandler(nil))
	}
	if contains(capabilities, "room.command") {
		registerHandler(roomworkloadhandlers.NewCommandHandler(nil))
	}
	if contains(capabilities, "room.stream") {
		registerHandler(roomworkloadhandlers.NewStreamHandler(nil))
	}
	directRoomTransfer := contains(capabilities, contracts.RoomTransferDirectCapability)
	e2eeRoomTransfer := contains(capabilities, contracts.RoomTransferE2EECapability)
	if directRoomTransfer != e2eeRoomTransfer {
		log.Fatal("room.transfer.direct.v1 and room.transfer.e2ee.v2 must be enabled together")
	}
	hybridRoomTransfer := contains(capabilities, contracts.RoomStorageCapability)
	if (directRoomTransfer || hybridRoomTransfer) && !contains(capabilities, contracts.RoomTransferCapability) {
		log.Fatal("room transfer endpoint capabilities require room.transfer")
	}
	if hybridRoomTransfer && (*roomStorageAddr == "" || *roomStorageAdvertiseURL == "") {
		log.Fatal("room.transfer.storage.v2 requires room-storage-addr and room-storage-advertise-url")
	}
	if directRoomTransfer || hybridRoomTransfer {
		transferHandler := roomtransferhandler.NewHandler(roomtransferhandler.Config{ListenAddress: *roomTransferAddr,
			AdvertiseURL: *roomTransferAdvertiseURL, StorageListenAddress: *roomStorageAddr, StorageAdvertiseURL: *roomStorageAdvertiseURL})
		if hybridRoomTransfer {
			if err := transferHandler.PrepareStorageListener(); err != nil {
				log.Fatal(err)
			}
		}
		defer transferHandler.Close()
		// Direct transfer sessions share one token-authenticated HTTP listener.
		// Keep that listener in the Worker process so all active lanes are
		// multiplexed on the advertised endpoint.
		if registerErr := registry.Register(transferHandler); registerErr != nil {
			log.Fatal(registerErr)
		}
	}
	store, err := runtime.OpenFileStore(*statePath)
	if err != nil {
		log.Fatal(err)
	}
	engine, err := runtime.NewEngine(identity.WorkerID, capabilities, registry, governor, store)
	if err != nil {
		log.Fatal(err)
	}
	workerServer, err := server.New(identity, capabilities, engine, governor, store, *token)
	if err != nil {
		log.Fatal(err)
	}
	httpServer := &http.Server{Handler: workerServer.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var receiptRecorder *evidence.Recorder
	if *wcpAddress != "" {
		receiptJournal, err := evidence.OpenFileJournal(*receiptState)
		if err != nil {
			log.Fatal(err)
		}
		receiptRecorder, err = evidence.NewRecorder(identity, nodePrivateKey, receiptJournal, store)
		if err != nil {
			log.Fatal(err)
		}
	}
	var advertisedCircuitEndpoint string
	if *circuitAddress != "" {
		table, err := circuit.OpenTable(*circuitState, identity)
		if err != nil {
			log.Fatal(err)
		}
		circuitService, err = circuit.NewService(identity, nodePrivateKey, table)
		if err != nil {
			log.Fatal(err)
		}
		boundEndpoint, err := circuitService.Listen(ctx, *circuitAddress)
		if err != nil {
			log.Fatal(err)
		}
		advertisedCircuitEndpoint = *circuitAdvertise
		if advertisedCircuitEndpoint == "" {
			advertisedCircuitEndpoint = boundEndpoint
		}
		if err := validateAdvertisedEndpoint(advertisedCircuitEndpoint); err != nil {
			log.Fatal(err)
		}
		workerServer.AttachCircuits(circuitService)
		log.Printf("Beam Circuit listening on %s advertised as %s", boundEndpoint, advertisedCircuitEndpoint)
	}
	httpErrors := make(chan error, 1)
	go func() { httpErrors <- httpServer.Serve(controlListener) }()
	if err := engine.Recover(ctx); err != nil {
		log.Fatal(err)
	}
	go engine.Reconcile(ctx, time.Second)
	if receiptRecorder != nil {
		go func() {
			if err := receiptRecorder.Run(ctx, engine.Results()); err != nil && ctx.Err() == nil {
				log.Printf("receipt recorder stopped: %v", err)
				stop()
			}
		}()
	}
	if *wcpAddress != "" {
		if *wcpCA == "" || *wcpServerName == "" {
			log.Fatal("WCP CA and TLS server name are required when WCP is enabled")
		}
		caBytes, err := os.ReadFile(*wcpCA)
		if err != nil {
			log.Fatal(err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caBytes) {
			log.Fatal("WCP CA does not contain a certificate")
		}
		delegationBytes, err := base64.RawURLEncoding.DecodeString(*delegation)
		if err != nil {
			log.Fatal("invalid Orchestrator delegation")
		}
		wcpClient, err := wcp.NewClient(wcp.ClientConfig{
			Address: *wcpAddress, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: *wcpServerName},
			PrivateKey: nodePrivateKey, Identity: identity, OrchestratorDelegation: delegationBytes,
			SoftwareVersion: version, Region: *region, Capabilities: capabilities,
			TotalResources: governor.Snapshot().Capacity, Engine: engine, Governor: governor, Store: store,
			CircuitEndpoint: advertisedCircuitEndpoint, Circuits: circuitService,
			Receipts: receiptRecorder,
		})
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			if err := wcpClient.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("WCP client stopped: %v", err)
				stop()
			}
		}()
	}
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
	}()
	log.Printf("Beam Worker %s worker_id=%s listening on %s capabilities=%s subsystem_mode=%s", version, identity.WorkerID, controlAddress, strings.Join(capabilities, ","), *subsystemMode)
	select {
	case <-ctx.Done():
	case err := <-httpErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}
}

func serveSubsystem(arguments []string) {
	flags := flag.NewFlagSet("subsystem", flag.ExitOnError)
	kind := flags.String("kind", "", "single workload kind served by this subprocess")
	_ = flags.Parse(arguments)
	config, err := subsystem.ConfigFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}
	handler, err := subsystem.BuildHandler(domain.Kind(*kind), config)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := subsystem.ServeOne(ctx, handler, os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envIntOrDefault(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDurationOrDefault(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func validateAdvertisedEndpoint(endpoint string) error {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("invalid advertised Circuit endpoint: %w", err)
	}
	if host == "" {
		return errors.New("advertised Circuit endpoint requires a reachable host")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return errors.New("--circuit-advertise is required when Circuit listens on an unspecified address")
	}
	return nil
}

func doctor(arguments []string) {
	flags := flag.NewFlagSet("doctor", flag.ExitOnError)
	workerID := flags.String("worker-id", os.Getenv("BEAM_WORKER_ID"), "canonical BeamCore worker_id")
	_ = flags.Parse(arguments)
	if *workerID == "" {
		log.Fatal("BEAM_WORKER_ID or --worker-id is required")
	}
	fmt.Printf("ok: worker_id=%s version=%s\n", *workerID, version)
}

func printNodeID(arguments []string) {
	flags := flag.NewFlagSet("node-id", flag.ExitOnError)
	nodeKeyPath := flags.String("node-key", "data/worker/node.key", "Ed25519 WCP node key")
	_ = flags.Parse(arguments)
	privateKey, err := wcp.LoadOrCreateNodeKey(*nodeKeyPath)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(wcp.NodeID(privateKey.Public().(ed25519.PublicKey)))
}

func splitList(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: beam-worker <serve|doctor|node-id|version> [options]")
}
