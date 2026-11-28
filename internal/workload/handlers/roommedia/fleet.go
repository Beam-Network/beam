package roommedia

import (
	"crypto/tls"
	"log"
	"net/http"
	"sync"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type Fleet interface {
	SendSessionEvent(event mediacontracts.SessionEvent) error
	SendUsageReport(message mediacontracts.UsageReportMessage) error
	ReportNodeLoad(workloadID string, load mediacontracts.NodeLoad)
	ForgetNode(workloadID string)
	EnsureHost(workloadID, hostname string) error
	EdgeHostname() string
	Capacity() *mediacontracts.WorkerCapacity
}

type TopologyWatch func(workloadID string, epoch, attempt uint64) (updates <-chan *mediacontracts.Topology, cancel func())

type NodeTLS struct {
	Addr           string
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	Certificate    func(hostname string) *tls.Certificate
	WrapHTTPS      func(http.Handler) http.Handler
}

var fleetMissing sync.Once

func (h *Handler) fleet() Fleet {
	if h.config.Fleet == nil {
		fleetMissing.Do(func() {
			log.Printf("room media: no media fleet is wired; node session events and usage reports are not sent")
		})
	}
	return h.config.Fleet
}
