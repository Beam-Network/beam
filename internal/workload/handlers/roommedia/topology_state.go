package roommedia

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const topologyStateFile = "topology.json"

func (s *AttemptState) saveView(view *mediacontracts.Topology) error {
	encoded, err := mediacontracts.MarshalWire(view)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.Dir, topologyStateFile), encoded)
}

var errNoSavedView = errors.New("no saved topology view")

func (s *AttemptState) loadView(offered *mediacontracts.Topology, now time.Time) (*mediacontracts.Topology, error) {
	if s == nil || s.Dir == "" {
		return nil, errNoSavedView
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, topologyStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoSavedView
	}
	if err != nil {
		return nil, err
	}
	view, err := mediacontracts.ValidateTopologyJSON(data)
	if err != nil {
		return nil, fmt.Errorf("invalid saved view: %w", err)
	}
	switch {
	case view.View == nil || offered.View == nil || view.View.NodeID != offered.View.NodeID:
		return nil, errors.New("the saved view is another node's")
	case view.SessionID != offered.SessionID:
		return nil, errors.New("the saved view is another session's")
	case view.Epoch < offered.Epoch || view.Epoch == offered.Epoch && view.Revision <= offered.Revision:
		return nil, fmt.Errorf("the saved view (epoch %d revision %d) is not newer than the offer's (epoch %d revision %d)",
			view.Epoch, view.Revision, offered.Epoch, offered.Revision)
	case now.UnixMilli() >= view.ExpiresAtMS:
		return nil, fmt.Errorf("the saved view (epoch %d revision %d) has expired", view.Epoch, view.Revision)
	}
	return view, nil
}
