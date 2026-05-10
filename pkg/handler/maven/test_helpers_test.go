package maven

import (
	"fmt"

	"github.com/yolocs/ocifactory/pkg/handler"
)

func newHandlerWithRegistry(registry handler.Registry, opts ...Option) (*Handler, error) {
	cfg := handlerConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if registry == nil {
		return nil, fmt.Errorf("registry must not be nil")
	}
	return &Handler{registryForNamespace: func(string) handler.Registry { return registry }, authMW: cfg.authMW}, nil
}
