package sfu

import (
	"net/http"
	"strings"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

type admission struct {
	id    string
	proof *mediacontracts.UsageProof
	grant *mediacontracts.ReceiptGrant
}

func (s *Session) admitted(r *http.Request, claims mediacontracts.TokenClaims, verifiedMS int64) admission {
	result := admission{id: usage.NewConnectionID()}
	if isNodeIdentity(claims.Identity) {
		return result
	}
	var settings *mediacontracts.UsageSettings
	if view := s.View(); view != nil {
		settings = view.Usage
	}
	if value := usage.RequestGrant(r); value != "" {
		if grant, ok := s.opts.Usage.BindGrant(value, settings, claims, verifiedMS); ok {
			result.grant, result.proof = &grant, usage.GrantProof(value, verifiedMS)
			return result
		}
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	result.proof = s.opts.Usage.TokenProof(strings.TrimSpace(token), settings, result.id, claims.Nonce, verifiedMS)
	return result
}
