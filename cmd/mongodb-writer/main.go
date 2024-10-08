// Copyright (c) Abstract Machines
// SPDX-License-Identifier: Apache-2.0

// Package main contains mongodb-writer main function to start the mongodb-writer service.
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
	"github.com/absmach/magistrala/consumers"
	consumertracing "github.com/absmach/magistrala/consumers/tracing"
	"github.com/absmach/magistrala/consumers/writers/api"
	mglog "github.com/absmach/magistrala/logger"
	jaegerclient "github.com/absmach/magistrala/pkg/jaeger"
	"github.com/absmach/magistrala/pkg/messaging/brokers"
	brokerstracing "github.com/absmach/magistrala/pkg/messaging/brokers/tracing"
	"github.com/absmach/magistrala/pkg/prometheus"
	"github.com/absmach/magistrala/pkg/server"
	httpserver "github.com/absmach/magistrala/pkg/server/http"
	"github.com/absmach/magistrala/pkg/uuid"
	"github.com/absmach/mg-contrib/consumers/writers/mongodb"
	mongoclient "github.com/absmach/mg-contrib/pkg/clients/mongo"
	"github.com/caarlos0/env/v10"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/sync/errgroup"
)

const (
	svcName        = "mongodb-writer"
	envPrefixDB    = "MG_MONGO_"
	envPrefixHTTP  = "MG_MONGO_WRITER_HTTP_"
	defSvcHTTPPort = "9008"
)

type config struct {
	LogLevel      string  `env:"MG_MONGO_WRITER_LOG_LEVEL"     envDefault:"info"`
	ConfigPath    string  `env:"MG_MONGO_WRITER_CONFIG_PATH"   envDefault:"/config.toml"`
	BrokerURL     string  `env:"MG_MESSAGE_BROKER_URL"         envDefault:"nats://localhost:4222"`
	JaegerURL     url.URL `env:"MG_JAEGER_URL"                 envDefault:"http://jaeger:14268/api/traces"`
	SendTelemetry bool    `env:"MG_SEND_TELEMETRY"             envDefault:"true"`
	InstanceID    string  `env:"MG_MONGO_WRITER_INSTANCE_ID"   envDefault:""`
	TraceRatio    float64 `env:"MG_JAEGER_TRACE_RATIO"         envDefault:"1.0"`
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	cfg := config{}
	if err := env.Parse(&cfg); err != nil {
		log.Fatalf("failed to load %s configuration : %s", svcName, err)
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

	db, err := mongoclient.Setup(envPrefixDB)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to setup mongo database : %s", err))
		exitCode = 1
		return
	}

	repo := newService(db, logger)
	repo = consumertracing.NewBlocking(tracer, repo, httpServerConfig)

	if err := consumers.Start(ctx, svcName, pubSub, repo, cfg.ConfigPath, logger); err != nil {
		logger.Error(fmt.Sprintf("failed to start MongoDB writer: %s", err))
		exitCode = 1
		return
	}

	hs := httpserver.NewServer(ctx, cancel, svcName, httpServerConfig, api.MakeHandler(svcName, cfg.InstanceID), logger)

	if cfg.SendTelemetry {
		chc := chclient.New(svcName, magistrala.Version, logger, cancel)
		go chc.CallHome(ctx)
	}

	g.Go(func() error {
		return hs.Start()
	})

	g.Go(func() error {
		return server.StopSignalHandler(ctx, cancel, logger, svcName, hs)
	})

	if err := g.Wait(); err != nil {
		logger.Error(fmt.Sprintf("MongoDB writer service terminated: %s", err))
	}
}

func newService(db *mongo.Database, logger *slog.Logger) consumers.BlockingConsumer {
	repo := mongodb.New(db)
	repo = api.LoggingMiddleware(repo, logger)
	counter, latency := prometheus.MakeMetrics("mongodb", "message_writer")
	repo = api.MetricsMiddleware(repo, counter, latency)
	return repo
}
