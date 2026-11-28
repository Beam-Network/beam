package roomworkloads

import "errors"

var ErrMediaWithheld = errors.New("room.media takes no new node on this Orchestrator: its plane is withdrawn")

func (m *Manager) WithholdMedia(withheld bool) { m.mediaNodes.withheld.Store(withheld) }

func (m *Manager) MediaWithheld() bool { return m.mediaNodes.withheld.Load() }
