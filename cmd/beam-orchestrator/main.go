package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/wcp"
	workerevidence "github.com/Beam-Network/beam/internal/evidence"
	"github.com/Beam-Network/beam/internal/orchestrator/connectors"
	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/payment"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/orchestrator/roomtransfer"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	orchestratorserver "github.com/Beam-Network/beam/internal/orchestrator/server"
	platformbittensor "github.com/Beam-Network/beam/internal/platform/bittensor"
	versioninfo "github.com/Beam-Network/beam/internal/version"
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
	default:
		usage()
		os.Exit(2)
	}
}

func serve(arguments []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	orchestratorID := flags.String("orchestrator-id", os.Getenv("BEAM_ORCHESTRATOR_ID"), "optional Orchestrator id override; generated and persisted when empty")
	hotkey := flags.String("hotkey", os.Getenv("BEAM_BITTENSOR_HOTKEY"), "optional Bittensor hotkey association")
	netuid := flags.Uint("netuid", 0, "Bittensor subnet netuid")
	addr := flags.String("addr", "127.0.0.1:8781", "owner-local Orchestrator API listen address")
	statePath := flags.String("state", "data/orchestrator/registry.json", "durable Orchestrator registry path")
	wcpAddr := flags.String("wcp-addr", os.Getenv("BEAM_WCP_LISTEN_ADDR"), "TLS WCP listen address; empty disables WCP")
	wcpCert := flags.String("wcp-tls-cert", os.Getenv("BEAM_WCP_TLS_CERT"), "WCP TLS certificate")
	wcpKey := flags.String("wcp-tls-key", os.Getenv("BEAM_WCP_TLS_KEY"), "WCP TLS private key")
	wcpJournal := flags.String("wcp-journal", "data/orchestrator/wcp-events.jsonl", "durable WCP command and result journal")
	taskState := flags.String("task-state", "data/orchestrator/tasks.json", "durable external task orchestration state")
	roomTransferState := flags.String("room-transfer-state", "data/orchestrator/room-transfers.json", "durable room transfer orchestration state")
	roomWorkloadState := flags.String("room-workload-state", "data/orchestrator/room-workloads", "durable generic room workload state directory")
	paymentState := flags.String("payment-state", "data/orchestrator/payment-evidence.json", "durable BeamCore payment evidence journal")
	paymentKey := flags.String("payment-key", "data/orchestrator/payment-attestation.key", "persistent Ed25519 payment attestation key")
	bittensorSocket := flags.String("bittensor-agent-socket", os.Getenv("BEAM_BITTENSOR_AGENT_SOCKET"), "optional Python Bittensor agent socket for legacy BeamCore signatures")
	ensureStreams := flags.Bool("nats-ensure-streams", false, "create missing JetStream task streams (development only)")
	beamCoreNATS := natsFlags(flags, "beamcore", "BEAMCORE", "", "", "", "")
	flags.StringVar(&beamCoreNATS.Environment, "beamcore-environment", envOrDefault("BEAM_ENV", "prod"), "BeamCore control environment")
	flags.StringVar(&beamCoreNATS.ControlPrefix, "beamcore-control-prefix", envOrDefault("BEAMCORE_CONTROL_PREFIX", "beam.orch.control"), "BeamCore orchestrator control subject prefix")
	flags.StringVar(&beamCoreNATS.GatewayURL, "beamcore-gateway-url", os.Getenv("BEAMCORE_GATEWAY_URL"), "public orchestrator URL registered with BeamCore")
	flags.StringVar(&beamCoreNATS.PublicAPIURL, "beamcore-public-api-url", os.Getenv("BEAM_PUBLIC_API_URL"), "BeamCore Public API URL used to classify duplicate control-session denials")
	beamCoreNATS.SoftwareVersion = version
	flags.StringVar(&beamCoreNATS.EvidenceSubject, "beamcore-nats-evidence-subject", "beam.workloads.beamcore.payment-evidence", "BeamCore payment evidence request/reply subject")
	roomTunnelCoordinatorURL := flags.String("room-tunnel-coordinator-url", os.Getenv("BEAM_ROOM_TUNNEL_COORDINATOR_URL"), "room tunnel coordinator HTTPS URL")
	studioNATS := natsFlags(flags, "studio", "BEAM_STUDIO", "BEAM_WORKFLOW_TASKS", "beam.workloads.studio.tasks",
		"beam.workloads.studio.results", "beam-orchestrator-studio")
	tunnelNATS := natsFlags(flags, "tunnel", "BEAM_TUNNEL", "TUNNEL_WORKLOADS", "beam.workloads.tunnel.tasks",
		"beam.workloads.tunnel.results", "beam-orchestrator-tunnel")
	flags.StringVar(&tunnelNATS.ProvisionSubject, "tunnel-nats-provision-subject", "beam.workloads.tunnel.provision", "Tunnel coordinator room lease request/reply subject")
	_ = flags.Parse(arguments)
	beamCoreNATS.Hotkey = *hotkey
	registryStore := registry.FileStateStore{Path: *statePath}
	resolvedOrchestratorID, generated, err := registry.ResolveOrchestratorID(*orchestratorID, registryStore)
	if err != nil {
		log.Fatal(err)
	}
	orchestrator := orchestratordomain.Orchestrator{OrchestratorID: resolvedOrchestratorID, Hotkey: *hotkey, NetUID: uint32(*netuid), Status: "active", CreatedAt: time.Now()}
	orchestratorRegistry, err := registry.NewWithStore(orchestrator, registryStore)
	if err != nil {
		log.Fatal(err)
	}
	if generated {
		log.Printf("generated and persisted Orchestrator identity orchestrator_id=%s", resolvedOrchestratorID)
	}
	var wcpServer *wcp.Server
	if *wcpAddr != "" {
		if *wcpCert == "" || *wcpKey == "" {
			log.Fatal("WCP TLS certificate and key are required when WCP is enabled")
		}
		journal, journalErr := wcp.OpenFileJournal(*wcpJournal)
		if journalErr != nil {
			log.Fatal(journalErr)
		}
		wcpServer, err = wcp.NewServerWithJournal(orchestrator.OrchestratorID, orchestratorRegistry, 1, journal)
		if err != nil {
			log.Fatal(err)
		}
	}
	var server *orchestratorserver.Server
	var tasks *dispatch.Service
	var payments *payment.Service
	var rooms *roomtransfer.Service
	var roomWorkloads *roomworkloads.Manager
	if wcpServer != nil {
		server = orchestratorserver.NewWithControl(orchestrator.OrchestratorID, orchestratorRegistry, wcpServer)
		store, storeErr := dispatch.OpenFileStore(*taskState)
		if storeErr != nil {
			log.Fatal(storeErr)
		}
		tasks, err = dispatch.NewService(dispatch.Config{OrchestratorID: orchestrator.OrchestratorID}, orchestratorRegistry, wcpServer, store)
		if err != nil {
			log.Fatal(err)
		}
		server.AttachOrchestration(tasks)
		roomStore, roomErr := roomtransfer.OpenFileStore(*roomTransferState)
		if roomErr != nil {
			log.Fatal(roomErr)
		}
		rooms, roomErr = roomtransfer.NewService(roomtransfer.Config{}, tasks, roomStore)
		if roomErr != nil {
			log.Fatal(roomErr)
		}
		roomWorkloads, roomErr = roomworkloads.OpenManager(*roomWorkloadState, tasks)
		if roomErr != nil {
			log.Fatal(roomErr)
		}
		attestationKey, keyErr := payment.LoadOrCreateKey(*paymentKey)
		if keyErr != nil {
			log.Fatal(keyErr)
		}
		paymentStore, paymentErr := payment.OpenFileStore(*paymentState)
		if paymentErr != nil {
			log.Fatal(paymentErr)
		}
		var legacySigner payment.LegacySigner
		if *bittensorSocket != "" {
			legacySigner, paymentErr = platformbittensor.NewClient(*bittensorSocket)
			if paymentErr != nil {
				log.Fatal(paymentErr)
			}
		}
		payments, paymentErr = payment.NewService(orchestrator.OrchestratorID, attestationKey, legacySigner, paymentStore, tasks)
		if paymentErr != nil {
			log.Fatal(paymentErr)
		}
	} else {
		server = orchestratorserver.New(orchestrator.OrchestratorID, orchestratorRegistry)
	}
	httpServer := &http.Server{Addr: *addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if tasks == nil && (beamCoreNATS.Enabled() || studioNATS.Enabled() || tunnelNATS.Enabled()) {
		log.Fatal("WCP must be enabled when a NATS workload connector is configured")
	}
	if tasks != nil {
		if *roomTunnelCoordinatorURL != "" {
			provisioner, provisionerErr := connectors.NewRoomTunnelHTTPProvisioner(*roomTunnelCoordinatorURL, nil)
			if provisionerErr != nil {
				log.Fatal(provisionerErr)
			}
			rooms.RegisterProvisioner(provisioner)
			roomWorkloads.RegisterProvisioner(provisioner)
		}
		beamCoreNATS.EnsureStream = *ensureStreams
		studioNATS.EnsureStream = *ensureStreams
		tunnelNATS.EnsureStream = *ensureStreams
		startConnectors(ctx, stop, tasks, payments, rooms, roomWorkloads, *beamCoreNATS, *studioNATS, *tunnelNATS)
		go replayDurable(ctx, tasks, payments, rooms, roomWorkloads)
	}
	if wcpServer != nil {
		if payments != nil {
			events := wcpServer.Journal().Events()
			for _, circuitFirst := range []bool{true, false} {
				for _, event := range events {
					if event.Receipt == nil || (event.Receipt.Type == workerevidence.ReceiptCircuit) != circuitFirst {
						continue
					}
					if err := payments.Observe(ctx, *event.Receipt); err != nil {
						log.Printf("restore Worker receipt for payment: %v", err)
					}
				}
			}
		}
		certificate, err := tls.LoadX509KeyPair(*wcpCert, *wcpKey)
		if err != nil {
			log.Fatal(err)
		}
		listener, err := net.Listen("tcp", *wcpAddr)
		if err != nil {
			log.Fatal(err)
		}
		tlsListener := tls.NewListener(listener, &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, NextProtos: []string{wcp.ALPN},
		})
		go func() {
			if err := wcpServer.Serve(ctx, tlsListener); err != nil && ctx.Err() == nil {
				log.Printf("WCP server stopped: %v", err)
				stop()
			}
		}()
		go func() {
			for event := range wcpServer.Results() {
				if tasks != nil {
					if err := tasks.HandleResult(ctx, event.Result); err != nil {
						log.Printf("persist/deliver Worker result: %v", err)
					}
				}
				log.Printf("Worker result worker_id=%s workload_id=%s state=%s", event.WorkerID, event.Result.WorkloadID, event.Result.State)
			}
		}()
		go func() {
			for event := range wcpServer.Progress() {
				if tasks != nil {
					if err := tasks.HandleProgressContext(ctx, event.Progress); err != nil {
						log.Printf("persist Worker progress: %v", err)
					}
				}
				log.Printf("Worker progress worker_id=%s workload_id=%s state=%s outputs=%v", event.WorkerID,
					event.Progress.WorkloadID, event.Progress.State, event.Progress.Outputs)
			}
		}()
		go func() {
			for event := range wcpServer.Checkpoints() {
				if rooms != nil {
					if err := rooms.DeliverCheckpoint(ctx, event.Checkpoint); err != nil {
						log.Printf("persist/deliver room transfer checkpoint: %v", err)
					}
				}
			}
		}()
		go func() {
			for event := range wcpServer.Receipts() {
				if payments != nil {
					if err := payments.Observe(ctx, event.Receipt); err != nil {
						log.Printf("aggregate Worker receipt for payment: %v", err)
					}
				}
				log.Printf("Worker receipt worker_id=%s receipt_id=%s type=%s event=%s", event.WorkerID,
					event.Receipt.ReceiptID, event.Receipt.Type, event.Receipt.Event)
			}
		}()
		log.Printf("Beam Orchestrator WCP listening on %s (TLS 1.3)", *wcpAddr)
	}
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
	}()
	log.Printf("Beam Orchestrator %s orchestrator_id=%s listening on %s", version, orchestrator.OrchestratorID, *addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

type connectorRunner interface {
	Run(context.Context) error
}

func startConnectors(ctx context.Context, stop context.CancelFunc, tasks *dispatch.Service, payments *payment.Service,
	rooms *roomtransfer.Service, roomWorkloads *roomworkloads.Manager, configs ...connectors.NATSConfig) {
	for index, config := range configs {
		if !config.Enabled() {
			continue
		}
		var runner connectorRunner
		var err error
		switch index {
		case 0:
			var connector *connectors.BeamCoreConnector
			connector, err = connectors.NewBeamCoreConnector(config, tasks, payments)
			if connector != nil {
				connector.AttachRoomTransfers(rooms)
				connector.AttachRoomWorkloads(roomWorkloads)
			}
			runner = connector
		case 1:
			runner, err = connectors.NewStudioConnector(config, tasks)
		case 2:
			var connector *connectors.TunnelConnector
			connector, err = connectors.NewTunnelConnector(config, tasks, rooms)
			if connector != nil {
				connector.AttachRoomWorkloads(roomWorkloads)
			}
			runner = connector
		}
		if err != nil {
			log.Fatal(err)
		}
		go func(name string, current connectorRunner) {
			log.Printf("starting %s NATS connector", name)
			if err := current.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("%s NATS connector stopped: %v", name, err)
				stop()
			}
		}(config.Name, runner)
	}
}

func replayDurable(ctx context.Context, tasks *dispatch.Service, payments *payment.Service, rooms *roomtransfer.Service,
	roomWorkloads *roomworkloads.Manager) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tasks.ReplayResults(ctx)
			if payments != nil {
				payments.Replay(ctx)
			}
			if rooms != nil {
				rooms.Replay(ctx)
			}
			if roomWorkloads != nil {
				roomWorkloads.Replay(ctx)
			}
		}
	}
}

func natsFlags(flags *flag.FlagSet, prefix, environmentPrefix, stream, task, result, durable string) *connectors.NATSConfig {
	config := &connectors.NATSConfig{}
	flags.StringVar(&config.URL, prefix+"-nats-url", os.Getenv(environmentPrefix+"_NATS_URL"), prefix+" NATS URL; empty disables this connector")
	flags.StringVar(&config.CredentialsFile, prefix+"-nats-creds", os.Getenv(environmentPrefix+"_NATS_CREDS"), prefix+" NATS credentials file")
	flags.StringVar(&config.Token, prefix+"-nats-token", os.Getenv(environmentPrefix+"_NATS_TOKEN"), prefix+" NATS token")
	flags.StringVar(&config.User, prefix+"-nats-user", os.Getenv(environmentPrefix+"_NATS_USER"), prefix+" NATS user")
	flags.StringVar(&config.Password, prefix+"-nats-password", os.Getenv(environmentPrefix+"_NATS_PASSWORD"), prefix+" NATS password")
	flags.StringVar(&config.Stream, prefix+"-nats-stream", stream, prefix+" JetStream task stream")
	flags.StringVar(&config.TaskSubject, prefix+"-nats-task-subject", task, prefix+" JetStream task subject")
	flags.StringVar(&config.ResultSubject, prefix+"-nats-result-subject", result, prefix+" result request/reply subject")
	flags.StringVar(&config.Durable, prefix+"-nats-durable", durable, prefix+" durable JetStream consumer")
	config.Name = "beam-orchestrator-" + prefix
	return config
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: beam-orchestrator <serve|version> [options]")
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
