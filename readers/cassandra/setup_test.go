// Copyright (c) Abstract Machines
// SPDX-License-Identifier: Apache-2.0

package cassandra_test

import (
	"fmt"
	"log"
	"os"
	"testing"

	mglog "github.com/absmach/magistrala/logger"
	casclient "github.com/absmach/mg-contrib/pkg/clients/cassandra"
	"github.com/gocql/gocql"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

var logger, _ = mglog.New(os.Stdout, "info")

func TestMain(m *testing.M) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		logger.Error(fmt.Sprintf("Could not connect to docker: %s", err))
	}

	container, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "cassandra",
		Tag:        "3.11.16",
	}, func(config *docker.HostConfig) {
		config.AutoRemove = true
		config.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		log.Fatalf("Could not start container: %s", err)
	}

	port := container.GetPort("9042/tcp")
	addr = fmt.Sprintf("%s:%s", addr, port)

	if err = pool.Retry(func() error {
		if err := createKeyspace([]string{addr}); err != nil {
			return err
		}

		session, err := casclient.Connect(casclient.Config{
			Hosts:    []string{addr},
			Keyspace: keyspace,
		})
		if err != nil {
			return err
		}
		defer session.Close()

		return nil
	}); err != nil {
		logger.Error(fmt.Sprintf("Could not connect to docker: %s", err))
	}

	code := m.Run()

	if err := pool.Purge(container); err != nil {
		logger.Error(fmt.Sprintf("Could not purge container: %s", err))
	}

	os.Exit(code)
}

func createKeyspace(hosts []string) error {
	cluster := gocql.NewCluster(hosts...)
	cluster.Consistency = gocql.Quorum

	session, err := cluster.CreateSession()
	if err != nil {
		return err
	}
	defer session.Close()

	keyspaceCQL := fmt.Sprintf(`CREATE KEYSPACE IF NOT EXISTS %s WITH replication =
                   {'class':'SimpleStrategy','replication_factor':'1'}`, keyspace)

	return session.Query(keyspaceCQL).Exec()
}
