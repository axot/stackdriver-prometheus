// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The main package for the Prometheus server executable.
package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof" // Comment this line to disable pprof endpoint.
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	kingpin "gopkg.in/alecthomas/kingpin.v2"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	conntrack "github.com/mwitkow/go-conntrack"
	"github.com/oklog/oklog/pkg/group"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	k8s_runtime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/jkohen/prometheus/retrieval"
	"github.com/jkohen/prometheus/stackdriver"
	"github.com/prometheus/common/promlog"
	promlogflag "github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
)

var (
	configSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "prometheus",
		Name:      "config_last_reload_successful",
		Help:      "Whether the last configuration reload attempt was successful.",
	})
	configSuccessTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "prometheus",
		Name:      "config_last_reload_success_timestamp_seconds",
		Help:      "Timestamp of the last successful configuration reload.",
	})
)

func init() {
	prometheus.MustRegister(version.NewCollector("prometheus"))
}

func main() {
	if os.Getenv("DEBUG") != "" {
		runtime.SetBlockProfileRate(20)
		runtime.SetMutexProfileFraction(20)
	}

	cfg := struct {
		configFile string

		prometheusURL string

		logLevel promlog.AllowedLevel
	}{}

	a := kingpin.New(filepath.Base(os.Args[0]), "The Prometheus monitoring server")

	a.Version(version.Print("prometheus"))

	a.HelpFlag.Short('h')

	a.Flag("config.file", "Prometheus configuration file path.").
		Default("prometheus.yml").StringVar(&cfg.configFile)

	promlogflag.AddFlags(a, &cfg.logLevel)

	_, err := a.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "Error parsing commandline arguments"))
		a.Usage(os.Args[1:])
		os.Exit(2)
	}

	logger := promlog.New(cfg.logLevel)

	// XXX(fabxc): Kubernetes does background logging which we can only customize by modifying
	// a global variable.
	// Ultimately, here is the best place to set it.
	k8s_runtime.ErrorHandlers = []func(error){
		func(err error) {
			level.Error(log.With(logger, "component", "k8s_client_runtime")).Log("err", err)
		},
	}

	level.Info(logger).Log("msg", "Starting Prometheus", "version", version.Info())
	level.Info(logger).Log("build_context", version.BuildContext())
	level.Info(logger).Log("host_details", Uname())
	level.Info(logger).Log("fd_limits", FdLimits())

	var (
		remoteStorage = stackdriver.NewStorage(log.With(logger, "component", "remote"))
	)

	var (
		discoveryManager = discovery.NewManager(log.With(logger, "component", "discovery manager"))
		scrapeManager    = retrieval.NewScrapeManager(log.With(logger, "component", "scrape manager"), remoteStorage)
	)

	// Monitor outgoing connections on default transport with conntrack.
	http.DefaultTransport.(*http.Transport).DialContext = conntrack.NewDialContextFunc(
		conntrack.DialWithTracing(),
	)

	reloaders := []func(cfg *config.Config) error{
		remoteStorage.ApplyConfig,
		discoveryManager.ApplyConfig,
		scrapeManager.ApplyConfig,
	}

	prometheus.MustRegister(configSuccess)
	prometheus.MustRegister(configSuccessTime)

	// TODO(jkohen): Expose the /metrics endpoint, so main_test can detect
	// that this server startup. We need this, because we don't support the
	// Prometheus web interface.

	// Start all components while we wait for TSDB to open but only load
	// initial config and mark ourselves as ready after it completed.
	dbOpen := make(chan struct{})
	// Wait until the server is ready to handle reloading
	reloadReady := make(chan struct{})

	var g group.Group
	{
		term := make(chan os.Signal)
		signal.Notify(term, os.Interrupt, syscall.SIGTERM)
		cancel := make(chan struct{})
		g.Add(
			func() error {
				select {
				case <-term:
					level.Warn(logger).Log("msg", "Received SIGTERM, exiting gracefully...")
				case <-cancel:
					break
				}
				return nil
			},
			func(err error) {
				close(cancel)
			},
		)
	}
	{
		ctxDiscovery, cancelDiscovery := context.WithCancel(context.Background())
		g.Add(
			func() error {
				err := discoveryManager.Run(ctxDiscovery)
				level.Info(logger).Log("msg", "Discovery manager stopped")
				return err
			},
			func(err error) {
				level.Info(logger).Log("msg", "Stopping discovery manager...")
				cancelDiscovery()
			},
		)
	}
	{
		g.Add(
			func() error {
				err := scrapeManager.Run(discoveryManager.SyncCh())
				level.Info(logger).Log("msg", "Scrape manager stopped")
				return err
			},
			func(err error) {
				// Scrape manager needs to be stopped before closing the TSDB
				// so that it doesn't try to write samples to a closed storage.
				level.Info(logger).Log("msg", "Stopping scrape manager...")
				scrapeManager.Stop()
			},
		)
	}
	{
		// Make sure that sighup handler is registered with a redirect to the channel before the potentially
		// long and synchronous tsdb init.
		hup := make(chan os.Signal)
		signal.Notify(hup, syscall.SIGHUP)
		cancel := make(chan struct{})
		g.Add(
			func() error {
				select {
				case <-reloadReady:
					break
				// In case a shutdown is initiated before the reloadReady is released.
				case <-cancel:
					return nil
				}

				for {
					select {
					case <-hup:
						if err := reloadConfig(cfg.configFile, logger, reloaders...); err != nil {
							level.Error(logger).Log("msg", "Error reloading config", "err", err)
						}
					case <-cancel:
						return nil
					}
				}

			},
			func(err error) {
				close(cancel)
			},
		)
	}
	{
		cancel := make(chan struct{})
		g.Add(
			func() error {
				select {
				case <-dbOpen:
					break
				// In case a shutdown is initiated before the dbOpen is released
				case <-cancel:
					return nil
				}

				if err := reloadConfig(cfg.configFile, logger, reloaders...); err != nil {
					return fmt.Errorf("Error loading config %s", err)
				}

				close(reloadReady)
				level.Info(logger).Log("msg", "Server is ready to receive requests.")
				<-cancel
				return nil
			},
			func(err error) {
				close(cancel)
			},
		)
	}
	{
		cancel := make(chan struct{})
		g.Add(
			func() error {
				// Any Stackdriver client initialization goes here.
				level.Info(logger).Log("msg", "Stackdriver client started")
				close(dbOpen)
				<-cancel
				return nil
			},
			func(err error) {
				if err := remoteStorage.Close(); err != nil {
					level.Error(logger).Log("msg", "Error stopping Stackdriver client", "err", err)
				}
				close(cancel)
			},
		)
	}
	if err := g.Run(); err != nil {
		level.Error(logger).Log("err", err)
	}
	level.Info(logger).Log("msg", "See you next time!")
}

func reloadConfig(filename string, logger log.Logger, rls ...func(*config.Config) error) (err error) {
	level.Info(logger).Log("msg", "Loading configuration file", "filename", filename)

	defer func() {
		if err == nil {
			configSuccess.Set(1)
			configSuccessTime.Set(float64(time.Now().Unix()))
		} else {
			configSuccess.Set(0)
		}
	}()

	conf, err := config.LoadFile(filename)
	if err != nil {
		return fmt.Errorf("couldn't load configuration (--config.file=%s): %v", filename, err)
	}

	failed := false
	for _, rl := range rls {
		if err := rl(conf); err != nil {
			level.Error(logger).Log("msg", "Failed to apply configuration", "err", err)
			failed = true
		}
	}
	if failed {
		return fmt.Errorf("one or more errors occurred while applying the new configuration (--config.file=%s)", filename)
	}
	return nil
}
