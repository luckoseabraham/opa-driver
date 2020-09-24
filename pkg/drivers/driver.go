package drivers

import (
	"context"
)

//QueryCfg configuration for QueryOpt.
type QueryCfg struct {
	TracingEnabled bool
}

//QueryOpt options for opa query.
type QueryOpt func(*QueryCfg)

//Driver driver for opa integration
type Driver interface {
	Init(ctx context.Context) error

	PutModule(ctx context.Context, name string, src string) error
	// PutModules upserts a number of modules under a given prefix.
	DeleteModule(ctx context.Context, name string) (bool, error)

	PutData(ctx context.Context, path string, data interface{}) error
	DeleteData(ctx context.Context, path string) (bool, error)

	Query(ctx context.Context, path string, input interface{}, opts ...QueryOpt) (*Response, error)

	Dump(ctx context.Context) (string, error)
}
