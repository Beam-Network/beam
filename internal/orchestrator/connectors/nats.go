package connectors

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

type NATSConfig struct {
	URL              string
	Name             string
	CredentialsFile  string
	Token            string
	User             string
	Password         string
	Stream           string
	TaskSubject      string
	ResultSubject    string
	EvidenceSubject  string
	ProvisionSubject string
	Durable          string
	EnsureStream     bool
	AckWait          time.Duration
	MaxDeliver       int
	RequestTimeout   time.Duration
	Environment      string
	ControlPrefix    string
	Hotkey           string
	GatewayURL       string
	SoftwareVersion  string
}

func (c NATSConfig) Enabled() bool { return c.URL != "" }

type natsConnector struct {
	config NATSConfig
	conn   *nats.Conn
	js     nats.JetStreamContext
}

func connectNATS(config NATSConfig) (*natsConnector, error) {
	if config.URL == "" {
		return nil, errors.New("NATS URL is required")
	}
	if config.TaskSubject != "" && (config.Stream == "" || config.Durable == "") {
		return nil, errors.New("NATS stream and durable consumer are required with a task subject")
	}
	if config.AckWait <= 0 {
		config.AckWait = 15 * time.Minute
	}
	if config.MaxDeliver <= 0 {
		config.MaxDeliver = 10
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 10 * time.Second
	}
	options := []nats.Option{nats.Name(config.Name), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second)}
	if config.CredentialsFile != "" {
		options = append(options, nats.UserCredentials(config.CredentialsFile))
	}
	if config.Token != "" {
		options = append(options, nats.Token(config.Token))
	}
	if config.User != "" || config.Password != "" {
		if config.User == "" || config.Password == "" || config.Token != "" || config.CredentialsFile != "" {
			return nil, errors.New("NATS user/password must be complete and cannot be combined with token or credentials file")
		}
		options = append(options, nats.UserInfo(config.User, config.Password))
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(config.URL)), "tls://") {
		// Production gateways (orch-gateway.b1m.ai:4222) expect the TLS handshake before the INFO line.
		options = append(options, nats.TLSHandshakeFirst())
	}
	connection, err := nats.Connect(config.URL, options...)
	if err != nil {
		return nil, fmt.Errorf("connect %s NATS: %w", config.Name, err)
	}
	var jetstream nats.JetStreamContext
	if config.TaskSubject != "" {
		jetstream, err = connection.JetStream(nats.MaxWait(10 * time.Second))
		if err != nil {
			connection.Close()
			return nil, err
		}
	}
	session := &natsConnector{config: config, conn: connection, js: jetstream}
	if config.EnsureStream && config.TaskSubject != "" {
		if err := session.ensureStream(); err != nil {
			connection.Close()
			return nil, err
		}
	}
	return session, nil
}

func (s *natsConnector) ensureStream() error {
	info, err := s.js.StreamInfo(s.config.Stream)
	if err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
		return err
	}
	if errors.Is(err, nats.ErrStreamNotFound) {
		_, err = s.js.AddStream(&nats.StreamConfig{Name: s.config.Stream, Subjects: []string{s.config.TaskSubject},
			Retention: nats.WorkQueuePolicy, Storage: nats.FileStorage, MaxAge: 7 * 24 * time.Hour})
		return err
	}
	if containsSubject(info.Config.Subjects, s.config.TaskSubject) {
		return nil
	}
	configuration := info.Config
	configuration.Subjects = append(configuration.Subjects, s.config.TaskSubject)
	_, err = s.js.UpdateStream(&configuration)
	return err
}

func (s *natsConnector) consume(ctx context.Context, handle func(context.Context, []byte) error) error {
	if s.config.TaskSubject == "" || s.js == nil {
		return errors.New("NATS task consumer is not configured")
	}
	subscription, err := s.js.PullSubscribe(s.config.TaskSubject, s.config.Durable,
		nats.BindStream(s.config.Stream), nats.ManualAck(), nats.AckExplicit(),
		nats.AckWait(s.config.AckWait), nats.MaxDeliver(s.config.MaxDeliver))
	if err != nil {
		return err
	}
	defer subscription.Unsubscribe()
	for {
		if ctx.Err() != nil {
			return nil
		}
		messages, err := subscription.Fetch(1, nats.MaxWait(time.Second))
		if errors.Is(err, nats.ErrTimeout) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, message := range messages {
			if err := handle(ctx, message.Data); err != nil {
				metadata, _ := message.Metadata()
				if metadata != nil && int(metadata.NumDelivered) >= s.config.MaxDeliver {
					_ = message.Term()
				} else {
					_ = message.NakWithDelay(time.Second)
				}
				continue
			}
			if err := message.AckSync(); err != nil {
				return err
			}
		}
	}
}

func (s *natsConnector) request(ctx context.Context, subject string, value []byte) ([]byte, error) {
	if subject == "" {
		return nil, errors.New("NATS request subject is required")
	}
	requestContext, cancel := context.WithTimeout(ctx, s.config.RequestTimeout)
	defer cancel()
	message, err := s.conn.RequestWithContext(requestContext, subject, value)
	if err != nil {
		return nil, err
	}
	return message.Data, nil
}

func (s *natsConnector) close() {
	if s.conn == nil {
		return
	}
	_ = s.conn.Drain()
	s.conn.Close()
}

func containsSubject(subjects []string, target string) bool {
	for _, subject := range subjects {
		if strings.TrimSpace(subject) == target {
			return true
		}
	}
	return false
}
