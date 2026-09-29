package roomtransfer

import (
	"net"
	"time"

	"github.com/Beam-Network/beam/internal/workload/handlers/workertls"
)

type workerCertificates = workertls.Certificates

func secureWorkerListener(listener net.Listener, now func() time.Time) (net.Listener, *workerCertificates) {
	return workertls.WrapListener(listener, now)
}
