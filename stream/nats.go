package stream

import (
	"github.com/maxpert/marmot/cfg"
	"github.com/nats-io/nats.go"
	"strings"
	"time"
)

func Connect() (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name(cfg.Config.NodeName()),
		nats.Timeout(60 * time.Second),
	}

	serverUrl := strings.Join(cfg.Config.NATS.URLs, ", ")
	if serverUrl == "" {
		server, err := startEmbeddedServer(cfg.Config.NodeName())
		if err != nil {
			return nil, err
		}

		opts = append(opts, nats.InProcessServer(server))
	}

	nc, err := nats.Connect(serverUrl, opts...)
	if err != nil {
		return nil, err
	}

	return nc, nil
}
