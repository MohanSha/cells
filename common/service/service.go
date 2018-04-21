/*
 * Copyright (c) 2018. Abstrium SAS <team (at) pydio.com>
 * This file is part of Pydio Cells.
 *
 * Pydio Cells is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio Cells is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio Cells.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */

// Package service acts as a factory for all Pydio services.
//
// Pydio services are wrapped around micro services with additional information and ability to declare themselves to the
// registry. Services can be of three main different type :
// - Generic Service : providing a Runner function, they can be used to package any kind of server library as a pydio service
// - Micro Service : GRPC-based services implementing specific protobuf-services
// - Web Service : Services adding more logic and exposing Rest APIs defined by the OpenAPI definitions generated from protobufs.
//
// Package provides additional aspects that can be added to any service and declared by "WithXXX" functions.
package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/gyuho/goraph"
	"github.com/micro/go-micro"
	"github.com/micro/go-micro/client"
	microregistry "github.com/micro/go-micro/registry"
	"github.com/micro/go-web"
	"go.uber.org/zap"

	"github.com/pydio/cells/common"
	"github.com/pydio/cells/common/boltdb"
	"github.com/pydio/cells/common/config"
	"github.com/pydio/cells/common/dao"
	"github.com/pydio/cells/common/forms"
	"github.com/pydio/cells/common/log"
	"github.com/pydio/cells/common/registry"
	"github.com/pydio/cells/common/service/context"
	proto "github.com/pydio/cells/common/service/proto"
	"github.com/pydio/cells/common/sql"
)

const (
	TYPE_GENERIC = iota
	TYPE_GRPC
	TYPE_REST
	TYPE_API
)

var (
	types = []string{"generic", "grpc", "rest", "api"}
)

type Service interface {
	registry.Service

	Init(...ServiceOption)
	Options() ServiceOptions
}

// Service for the pydio app
type service struct {
	// Computed by external functions during listing operations
	nodes    []*microregistry.Node
	excluded bool

	opts ServiceOptions
	node goraph.Node
}

// Checker is a function that checks if the service is correctly Running
type Checker interface {
	Check() error
}

type CheckerFunc func() error

// Check implements the Chercker interface
func (f CheckerFunc) Check() error {
	return f()
}

type Runner interface {
	Run() error
}

type RunnerFunc func() error

func (f RunnerFunc) Run() error {
	return f()
}

type Stopper interface {
	Stop() error
}

type StopperFunc func() error

func (f StopperFunc) Stop() error {
	return f()
}

type StopFunctionKey struct{}

// HandlerProvider returns a handler function from a micro service
type HandlerProvider func(micro.Service) interface{}

// NewService provides everything needed to run a service, no matter the type
func NewService(opts ...ServiceOption) Service {

	s := &service{
		opts: newOptions(opts...),
	}

	// Setting context
	ctx, cancel := context.WithCancel(context.Background())

	ctx = servicecontext.WithServiceName(ctx, s.opts.Name)

	if s.IsGRPC() {
		ctx = servicecontext.WithServiceColor(ctx, 31)
	} else if s.IsREST() {
		ctx = servicecontext.WithServiceColor(ctx, 32)
	} else {
		ctx = servicecontext.WithServiceColor(ctx, 34)
	}

	// Setting config
	s.Init(
		Context(ctx),
		Cancel(cancel),
		Version(common.Version().String()),

		// Adding the config to the context
		AfterInit(func(_ Service) error {
			cfg := make(config.Map)

			if err := config.Get("services", s.Name()).Scan(&cfg); err != nil {
				log.Logger(ctx).Error("", zap.Error(err))
				return err
			}

			if cfg == nil {
				cfg = make(config.Map)
			}

			// Retrieving and assigning port to the config
			if p := config.Get("ports", s.Name()).Int(0); p != 0 {
				cfg.Set("port", p)
			}

			log.Logger(ctx).Debug("Service configuration retrieved", zap.String("service", s.Name()), zap.Any("cfg", cfg))
			ctx = servicecontext.WithConfig(ctx, cfg)

			s.Init(Context(ctx))

			return nil
		}),

		AfterInit(func(_ Service) error {
			// TODO :  WHY IS THAT ?
			// utils.SaveConfigs()

			return nil
		}),

		// Adding the dao to the context
		BeforeStart(func(_ Service) error {

			// Only if we have a DAO
			if s.Options().DAO == nil {
				return nil
			}

			var d dao.DAO
			driver, dsn := config.GetDatabase(s.Name())

			var prefix string
			switch v := s.Options().Prefix.(type) {
			case func(Service) string:
				prefix = v(s)
			case string:
				prefix = v
			default:
				prefix = ""
			}

			switch driver {
			case "mysql":
				d = s.Options().DAO(sql.NewDAO(driver, dsn, prefix))
			case "sqlite3":
				d = s.Options().DAO(sql.NewDAO(driver, dsn, prefix))
			case "boltdb":
				d = s.Options().DAO(boltdb.NewDAO(driver, dsn, prefix))
			default:
				return fmt.Errorf("Wrong type of driver")
			}

			if d == nil {
				return fmt.Errorf("This driver is not implemented for this service")
			}

			ctx = servicecontext.WithDAO(ctx, d)

			s.Init(Context(ctx))

			return nil

		}),

		// Adding a check before starting the service to ensure all dependencies are running
		BeforeStart(func(_ Service) error {
			for _, d := range s.Options().Dependencies {
				Retry(func() error {
					runningServices, err := registry.ListRunningServices()
					if err != nil {
						return err
					}

					for _, r := range runningServices {
						if d.Name == r.Name() {
							return nil
						}
					}

					return fmt.Errorf("not found")
				})
			}

			return nil
		}),

		// Checking the service is running
		AfterStart(func(_ Service) error {
			tick := time.Tick(10 * time.Millisecond)

			for {
				select {
				case <-ctx.Done():
					// We have stopped properly - errorr should be logged elsewhere if there was one
					return nil
				case <-tick:
					if s.IsRunning() {
						return nil
					}
				}
			}
		}),
	)

	// Finally, register on the main app registry
	s.Options().Registry.Register(s)

	return s
}

func (s *service) Init(opts ...ServiceOption) {
	// process options
	for _, o := range opts {
		o(&s.opts)
	}
}

func (s *service) Options() ServiceOptions {
	return s.opts
}

func (s *service) BeforeInit() error {
	for _, f := range s.Options().BeforeInit {
		if err := f(s); err != nil {
			return err
		}
	}

	return nil
}

func (s *service) AfterInit() error {
	for _, f := range s.Options().AfterInit {
		if err := f(s); err != nil {
			return err
		}
	}

	return nil
}

// Start a service and its dependencies
func (s *service) Start() {

	ctx := s.Options().Context
	cancel := s.Options().Cancel

	for _, f := range s.Options().BeforeStart {
		if err := f(s); err != nil {
			log.Logger(ctx).Error("Could not prepare start ", zap.Error(err))
			cancel()
		}
	}

	if s.Options().Micro != nil {
		go func() {
			if err := s.Options().MicroInit(s); err != nil {
				log.Logger(ctx).Error("Could not micro init ", zap.Error(err))
				cancel()
			}
			if err := s.Options().Micro.Run(); err != nil {
				log.Logger(ctx).Error("Could not run ", zap.Error(err))
				cancel()
			}
		}()
	}

	if s.Options().Web != nil {
		go func() {
			if err := s.Options().Web.Run(); err != nil {
				log.Logger(ctx).Error("Could not run ", zap.Error(err))
				cancel()
			}
		}()
	}

	for _, f := range s.Options().AfterStart {
		if err := f(s); err != nil {
			log.Logger(ctx).Error("Could not finalize start ", zap.Error(err))
			cancel()
		}
	}
}

// Start a service and its dependencies
func (s *service) Stop() {

	ctx := s.Options().Context
	cancel := s.Options().Cancel

	for _, f := range s.Options().BeforeStop {
		if err := f(s); err != nil {
			log.Logger(ctx).Error("Could not prepare stop ", zap.Error(err))
		}
	}

	// Cancelling context should stop the service altogether
	cancel()

	for _, f := range s.Options().AfterStop {
		if err := f(s); err != nil {
			log.Logger(ctx).Error("Could not finalize stop ", zap.Error(err))
		}
	}
}

// IsRunning provides a quikc way to check that a service is running
func (s *service) IsRunning() bool {
	ctx := s.getContext()

	if err := s.Check(ctx); err != nil {
		return false
	}
	return true
}

// Check the status of the service (globally - not specific to an endpoint)
func (s *service) Check(ctx context.Context) error {
	running, err := registry.ListRunningServices()
	if err != nil {

		return err
	}

	for _, r := range running {
		if s.Name() == r.Name() {
			return nil
		}
	}

	return fmt.Errorf("Not found")
}

func (s *service) AddDependency(name string) {
	if name == s.Name() {
		return
	}
	s.Init(Dependency(name, []string{""}))
}

func (s *service) GetDependencies() []registry.Service {

	var r []registry.Service

	for _, d := range s.Options().Dependencies {
		for _, rr := range s.Options().Registry.GetServicesByName(d.Name) {
			r = append(r, rr)
		}
	}

	return r
}

func (s *service) Name() string {
	return s.Options().Name
}

func (s *service) Tags() []string {
	return s.Options().Tags
}

func (s *service) Version() string {
	return s.Options().Version
}

func (s *service) Description() string {
	return s.Options().Description
}

func (s *service) Regexp() *regexp.Regexp {
	return s.Options().Regexp
}

func (s *service) SetExcluded(ex bool) {
	s.excluded = ex
}

func (s *service) IsExcluded() bool {
	return s.excluded
}

func (s *service) SetRunningNodes(nodes []*microregistry.Node) {
	s.nodes = nodes
}

func (s *service) RunningNodes() []*microregistry.Node {
	return s.nodes
}

func (s *service) ExposedConfigs() *forms.Form {
	return s.Options().ExposedConfigs
}

func (s *service) IsGeneric() bool {
	return (s.Options().Micro != nil && !strings.HasPrefix(s.Name(), common.SERVICE_GRPC_NAMESPACE_))
}

func (s *service) IsGRPC() bool {
	return s.Options().Micro != nil && strings.HasPrefix(s.Name(), common.SERVICE_GRPC_NAMESPACE_)
}

func (s *service) IsREST() bool {
	return s.Options().Web != nil
}

func (s *service) Client() (string, client.Client) {
	return s.Options().Micro.Server().Options().Name, s.Options().Micro.Client()
}

func (s *service) MatchesRegexp(o string) bool {
	if reg := s.Options().Regexp; reg != nil && reg.MatchString(o) {
		if matches := reg.FindStringSubmatch(o); len(matches) == 2 {
			s.Init(
				Name(matches[0]),
				Source(matches[1]),
			)

			return true
		}
	}

	return false
}

func (s *service) getContext() context.Context {
	// if m, ok := (s.micro).(micro.Service); ok {
	// 	return m.Options().Context
	// } else if w, ok := (s.micro).(web.Service); ok {
	// 	return w.Options().Context
	// }

	return nil
}

// RestHandlerBuilder builds a RestHandler
type RestHandlerBuilder func(service web.Service, defaultClient client.Client) interface{}

type Handler struct {
	service micro.Service
}

// Status of the service - If we reach this point, it means that this micro service is correctly up and running
func (h *Handler) Status(ctx context.Context, in *empty.Empty, out *proto.StatusResponse) error {
	out.OK = true

	return nil
}