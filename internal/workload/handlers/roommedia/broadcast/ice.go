package broadcast

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

func ICEServers(config Config, nonce string, now time.Time) mc.ICEResponse {
	ttl := config.TURNCredentialTTL
	if ttl <= 0 {
		ttl = 3 * time.Hour
	}
	expires := now.Add(ttl)
	prefix := strings.NewReplacer(":", "_", " ", "_").Replace(strings.TrimSpace(config.TURNUsernamePrefix))
	if prefix == "" {
		prefix = "beam"
	}
	servers := make([]mc.ICEServer, 0, len(config.ICEServers))
	for _, server := range config.ICEServers {
		entry := mc.ICEServer{URLs: []string{server}}
		lower := strings.ToLower(strings.TrimSpace(server))
		if secret := strings.TrimSpace(config.TURNSecret); secret != "" && (strings.HasPrefix(lower, "turn:") || strings.HasPrefix(lower, "turns:")) {
			username := fmt.Sprintf("%d:%s-%s", expires.Unix(), prefix, nonce)
			digest := hmac.New(sha1.New, []byte(secret))
			_, _ = digest.Write([]byte(username))
			credential := base64.StdEncoding.EncodeToString(digest.Sum(nil))
			entry.Username, entry.Credential = &username, &credential
		}
		servers = append(servers, entry)
		if len(servers) == 8 {
			break
		}
	}
	return mc.ICEResponse{ICEServers: servers, ExpiresAtMS: expires.UnixMilli()}
}
