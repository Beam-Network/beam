package media

import (
	"github.com/Beam-Network/beam/internal/worker/mediafleet"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	roommediahandler "github.com/Beam-Network/beam/internal/workload/handlers/roommedia"
)

var _ roommediahandler.Fleet = (*mediafleet.Fleet)(nil)

func withMediaFleet(config roommediahandler.Config, fleet *mediafleet.Fleet) roommediahandler.Config {
	if fleet == nil {
		return config
	}
	certificates := fleet.Certificates()
	config.Fleet = fleet
	config.WatchTopology = mediaTopologyViews(fleet)
	config.NodeTLS = &roommediahandler.NodeTLS{Addr: fleet.Config().HTTPSAddr,
		GetCertificate: certificates.GetCertificate, Certificate: certificates.Certificate, WrapHTTPS: fleet.WrapHTTPS}
	config.StateDir = fleet.Config().StateDir
	return config
}

func mediaTopologyViews(fleet *mediafleet.Fleet) roommediahandler.TopologyWatch {
	return func(workloadID string, epoch, attempt uint64) (<-chan *mediacontracts.Topology, func()) {
		updates, cancel := fleet.WatchTopology(workloadID)
		views := make(chan *mediacontracts.Topology, 1)
		go func() {
			defer close(views)
			for update := range updates {
				if update.Wire.Epoch != epoch || update.Wire.Attempt != attempt {
					continue
				}
				select {
				case views <- update.Topology:
				default:
					select {
					case <-views:
					default:
					}
					views <- update.Topology
				}
			}
		}()
		return views, cancel
	}
}
