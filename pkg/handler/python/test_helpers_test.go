package python

import (
	"fmt"

	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/renderer"
)

func newHandlerWithRegistry(registry handler.Registry, opts ...Option) (*Handler, error) {
	cfg := handlerConfig{
		simpleIndexCacheTTL: DefaultSimpleIndexCacheTTL,
		maxUploadBytes:      DefaultMaxUploadBytes,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if registry == nil {
		return nil, fmt.Errorf("registry must not be nil")
	}
	r, err := renderer.New(fs)
	if err != nil {
		return nil, fmt.Errorf("failed to create renderer: %w", err)
	}
	return &Handler{
		registryForNamespace: func(string) handler.Registry { return registry },
		renderer:             r,
		indexCache:           newSimpleIndexCache(cfg.simpleIndexCacheTTL),
		authMW:               cfg.authMW,
		maxUploadBytes:       cfg.maxUploadBytes,
	}, nil
}
