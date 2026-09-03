package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const timeLayout = time.RFC3339Nano

var now = time.Now

func acknowledged(ctx context.Context, session *natsConnector, subject string, payload []byte, authority string) error {
	response, err := session.request(ctx, subject, payload)
	if err != nil {
		return err
	}
	if len(response) == 0 {
		return errors.New(authority + " returned an empty acknowledgement")
	}
	var ack struct {
		Acknowledged bool   `json:"acknowledged"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal(response, &ack); err != nil {
		return err
	}
	if !ack.Acknowledged {
		return errors.New(fallback(ack.Reason, authority+" did not acknowledge result"))
	}
	return nil
}

func fallback(value, other string) string {
	if value == "" {
		return other
	}
	return value
}
