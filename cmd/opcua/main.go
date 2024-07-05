// Copyright (c) Abstract Machines
// SPDX-License-Identifier: Apache-2.0

// Package main contains opcua-adapter main function to start the opcua-adapter service.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"

	chclient "github.com/absmach/callhome/pkg/client"
	"github.com/absmach/magistrala"
	mglog "github.com/absmach/magistrala/logger"
	"github.com/absmach/magistrala/pkg/events"
	"github.com/absmach/magistrala/pkg/events/store"
	jaegerclient "github.com/absmach/magistrala/pkg/jaeger"
	"github.com/absmach/magistrala/pkg/messaging/brokers"
	brokerstracing "github.com/absmach/magistrala/pkg/messaging/brokers/tracing"
	"github.com/absmach/magistrala/pkg/prometheus"
	"github.com/absmach/magistrala/pkg/server"
	httpserver "github.com/absmach/magistrala/pkg/server/http"
	"github.com/absmach/magistrala/pkg/uuid"
	"github.com/absmach/mg-contrib/opcua"
	"github.com/absmach/mg-contrib/opcua/api"
	"github.com/absmach/mg-contrib/opcua/db"
	opcuaevents "github.com/absmach/mg-contrib/opcua/events"
	"github.com/absmach/mg-contrib/opcua/gopcua"
	redisclient "github.com/absmach/mg-contrib/pkg/clients/redis"
	"github.com/caarlos0/env/v10"
	"github.com/go-redis/redis/v8"
	"golang.org/x/sync/errgroup"
)

const (
	svcName        = "opc-ua-adapter"
	envPrefixHTTP  = "MG_OPCUA_ADAPTER_HTTP_"
	defSvcHTTPPort = "8180"

	thingsRMPrefix     = "thing"
	channelsRMPrefix   = "channel"
	connectionRMPrefix = "connection"

	thingsStream = "events.magistrala.things"
)

type config struct {
	LogLevel       string  `env:"MG_OPCUA_ADAPTER_LOG_LEVEL"          envDefault:"info"`
	ESConsumerName string  `env:"MG_OPCUA_ADAPTER_EVENT_CONSUMER"     envDefault:"opcua-adapter"`
	BrokerURL      string  `env:"MG_MESSAGE_BROKER_URL"               envDefault:"nats://localhost:4222"`
	JaegerURL      url.URL `env:"MG_JAEGER_URL"                       envDefault:"http://localhost:14268/api/traces"`
	SendTelemetry  bool    `env:"MG_SEND_TELEMETRY"                   envDefault:"true"`
	InstanceID     string  `env:"MG_OPCUA_ADAPTER_INSTANCE_ID"        envDefault:""`
	ESURL          string  `env:"MG_ES_URL"                           envDefault:"nats://localhost:4222"`
	RouteMapURL    string  `env:"MG_OPCUA_ADAPTER_ROUTE_MAP_URL"      envDefault:"redis://localhost:6379/0"`
	TraceRatio     float64 `env:"MG_JAEGER_TRACE_RATIO"               envDefault:"1.0"`
}

func main() {
	ctx, httpCancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	cfg := config{}
	if err := env.Parse(&cfg); err != nil {
		log.Fatalf("failed to load %s configuration : %s", svcName, err)
	}

	opcConfig := opcua.Config{}
	if err := env.Parse(&opcConfig); err != nil {
		log.Fatalf("failed to load %s opcua client configuration : %s", svcName, err)
	}

	logger, err := mglog.New(os.Stdout, cfg.LogLevel)
	if err != nil {
		log.Fatalf("failed to init logger: %s", err.Error())
	}

	var exitCode int
	defer mglog.ExitWithError(&exitCode)

	if cfg.InstanceID == "" {
		if cfg.InstanceID, err = uuid.New().ID(); err != nil {
			logger.Error(fmt.Sprintf("failed to generate instanceID: %s", err))
			exitCode = 1
			return
		}
	}

	httpServerConfig := server.Config{Port: defSvcHTTPPort}
	if err := env.ParseWithOptions(&httpServerConfig, env.Options{Prefix: envPrefixHTTP}); err != nil {
		logger.Error(fmt.Sprintf("failed to load %s HTTP server configuration : %s", svcName, err))
		exitCode = 1
		return
	}

	rmConn, err := redisclient.Connect(cfg.RouteMapURL)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to setup %s bootstrap event store redis client : %s", svcName, err))
		exitCode = 1
		return
	}
	defer rmConn.Close()

	thingRM := newRouteMapRepositoy(rmConn, thingsRMPrefix, logger)
	chanRM := newRouteMapRepositoy(rmConn, channelsRMPrefix, logger)
	connRM := newRouteMapRepositoy(rmConn, connectionRMPrefix, logger)

	tp, err := jaegerclient.NewProvider(ctx, svcName, cfg.JaegerURL, cfg.InstanceID, cfg.TraceRatio)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to init Jaeger: %s", err))
		exitCode = 1
		return
	}
	defer func() {
		if err := tp.Shutdown(ctx); err != nil {
			logger.Error(fmt.Sprintf("Error shutting down tracer provider: %v", err))
		}
	}()
	tracer := tp.Tracer(svcName)

	pubSub, err := brokers.NewPubSub(ctx, cfg.BrokerURL, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to connect to message broker: %s", err))
		exitCode = 1
		return
	}
	defer pubSub.Close()
	pubSub = brokerstracing.NewPubSub(httpServerConfig, tracer, pubSub)

	sub := gopcua.NewSubscriber(ctx, pubSub, thingRM, chanRM, connRM, logger)
	browser := gopcua.NewBrowser(ctx, logger)

	svc := newService(sub, browser, thingRM, chanRM, connRM, opcConfig, logger)

	go subscribeToStoredSubs(ctx, sub, opcConfig, logger)

	if err = subscribeToThingsES(ctx, svc, cfg, logger); err != nil {
		logger.Error(fmt.Sprintf("failed to subscribe to things event store: %s", err))
		exitCode = 1
		return
	}

	logger.Info("Subscribed to Event Store")

	hs := httpserver.NewServer(ctx, httpCancel, svcName, httpServerConfig, api.MakeHandler(svc, logger, cfg.InstanceID), logger)

	if cfg.SendTelemetry {
		chc := chclient.New(svcName, magistrala.Version, logger, httpCancel)
		go chc.CallHome(ctx)
	}

	g.Go(func() error {
		return hs.Start()
	})

	g.Go(func() error {
		return server.StopSignalHandler(ctx, httpCancel, logger, svcName, hs)
	})

	if err := g.Wait(); err != nil {
		logger.Error(fmt.Sprintf("OPC-UA adapter service terminated: %s", err))
	}
}

func subscribeToStoredSubs(ctx context.Context, sub opcua.Subscriber, cfg opcua.Config, logger *slog.Logger) {
	// Get all stored subscriptions
	nodes, err := db.ReadAll()
	if err != nil {
		logger.Warn(fmt.Sprintf("Read stored subscriptions failed: %s", err))
	}

	for _, n := range nodes {
		cfg.ServerURI = n.ServerURI
		cfg.NodeID = n.NodeID
		go func() {
			if err := sub.Subscribe(ctx, cfg); err != nil {
				logger.Warn(fmt.Sprintf("Subscription failed: %s", err))
			}
		}()
	}
}

func subscribeToThingsES(ctx context.Context, svc opcua.Service, cfg config, logger *slog.Logger) error {
	subscriber, err := store.NewSubscriber(ctx, cfg.ESURL, logger)
	if err != nil {
		return err
	}

	subConfig := events.SubscriberConfig{
		Stream:   thingsStream,
		Consumer: cfg.ESConsumerName,
		Handler:  opcuaevents.NewEventHandler(svc),
	}
	return subscriber.Subscribe(ctx, subConfig)
}

func newRouteMapRepositoy(client *redis.Client, prefix string, logger *slog.Logger) opcua.RouteMapRepository {
	logger.Info(fmt.Sprintf("Connected to %s Redis Route-map", prefix))
	return opcuaevents.NewRouteMapRepository(client, prefix)
}

func newService(sub opcua.Subscriber, browser opcua.Browser, thingRM, chanRM, connRM opcua.RouteMapRepository, opcuaConfig opcua.Config, logger *slog.Logger) opcua.Service {
	svc := opcua.New(sub, browser, thingRM, chanRM, connRM, opcuaConfig, logger)
	svc = api.LoggingMiddleware(svc, logger)
	counter, latency := prometheus.MakeMetrics("opc_ua_adapter", "api")
	svc = api.MetricsMiddleware(svc, counter, latency)

	return svc
}
