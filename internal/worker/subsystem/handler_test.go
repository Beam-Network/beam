package subsystem

import (
	"testing"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestBuildHandlerIsolatesSourceDigests(t *testing.T) {
	handler, err := BuildHandler(domain.KindTransferSourceDigest, Config{ControlToken: "secret"}.ForKind(domain.KindTransferSourceDigest))
	if err != nil || handler.Kind() != domain.KindTransferSourceDigest {
		t.Fatalf("source digest has no isolated handler: %v", err)
	}
	if config := (Config{ControlToken: "secret"}).ForKind(domain.KindTransferSourceDigest); config.ControlToken != "" {
		t.Fatal("source digest subprocess received unrelated configuration")
	}
}
