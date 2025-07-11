/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/kelseyhightower/envconfig"
	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	// Injection related imports.
	network "knative.dev/networking/pkg"
	netcfg "knative.dev/networking/pkg/config"
	netprobe "knative.dev/networking/pkg/http/probe"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	configmapinformer "knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/injection/sharedmain"
	pkglogging "knative.dev/pkg/logging"
	"knative.dev/pkg/logging/logkey"
	pkgnet "knative.dev/pkg/network"
	k8sruntime "knative.dev/pkg/observability/runtime/k8s"
	"knative.dev/pkg/signals"
	"knative.dev/pkg/system"
	"knative.dev/pkg/version"
	"knative.dev/pkg/websocket"
	"knative.dev/serving/pkg/activator"
	"knative.dev/serving/pkg/activator/certificate"
	activatorconfig "knative.dev/serving/pkg/activator/config"
	activatorhandler "knative.dev/serving/pkg/activator/handler"
	activatornet "knative.dev/serving/pkg/activator/net"
	apiconfig "knative.dev/serving/pkg/apis/config"
	asmetrics "knative.dev/serving/pkg/autoscaler/metrics"
	pkghttp "knative.dev/serving/pkg/http"
	"knative.dev/serving/pkg/http/handler"
	"knative.dev/serving/pkg/logging"
	"knative.dev/serving/pkg/networking"
	o11yconfigmap "knative.dev/serving/pkg/observability/configmap"
	"knative.dev/serving/pkg/observability/otel"
)

const (
	component = "activator"

	// The port on which autoscaler WebSocket server listens.
	autoscalerPort = ":8080"
)

type config struct {
	PodName string `split_words:"true" required:"true"`
	PodIP   string `split_words:"true" required:"true"`

	// These are here to allow configuring higher values of keep-alive for larger environments.
	// TODO: run loadtests using these flags to determine optimal default values.
	MaxIdleProxyConns        int `split_words:"true" default:"1000"`
	MaxIdleProxyConnsPerHost int `split_words:"true" default:"100"`
}

func main() {
	// Set up a context that we can cancel to tell informers and other subprocesses to stop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := injection.ParseAndGetRESTConfigOrDie()

	log.Printf("Registering %d clients", len(injection.Default.GetClients()))
	log.Printf("Registering %d informer factories", len(injection.Default.GetInformerFactories()))
	log.Printf("Registering %d informers", len(injection.Default.GetInformers()))

	ctx, informers := injection.Default.SetupInformers(ctx, cfg)

	var env config
	if err := envconfig.Process("", &env); err != nil {
		log.Fatal("Failed to process env: ", err)
	}

	kubeClient := kubeclient.Get(ctx)

	// We sometimes startup faster than we can reach kube-api. Poll on failure to prevent us terminating
	var err error
	if perr := wait.PollUntilContextTimeout(ctx, time.Second, 60*time.Second, true, func(context.Context) (bool, error) {
		if err = version.CheckMinimumVersion(kubeClient.Discovery()); err != nil {
			log.Print("Failed to get k8s version ", err)
		}
		return err == nil, nil
	}); perr != nil {
		log.Fatal("Timed out attempting to get k8s version: ", err)
	}

	// Set up our logger.
	loggingConfig, err := sharedmain.GetLoggingConfig(ctx)
	if err != nil {
		log.Fatal("Error loading/parsing logging configuration: ", err)
	}

	logger, atomicLevel := pkglogging.NewLoggerFromConfig(loggingConfig, component)
	logger = logger.With(
		zap.String(logkey.ControllerType, component),
		zap.String(logkey.Pod, env.PodName),
	)
	ctx = pkglogging.WithLogger(ctx, logger)
	defer flush(logger)

	pprof := k8sruntime.NewProfilingServer(logger.Named("pprof"))

	mp, tp := otel.SetupObservabilityOrDie(ctx, "activator", logger, pprof)

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := mp.Shutdown(ctx); err != nil {
			logger.Errorw("Error flushing metrics", zap.Error(err))
		}
		if err := tp.Shutdown(ctx); err != nil {
			logger.Errorw("Error flushing traces", zap.Error(err))
		}
	}()

	// Run informers instead of starting them from the factory to prevent the sync hanging because of empty handler.
	if err := controller.StartInformers(ctx.Done(), informers...); err != nil {
		logger.Fatalw("Failed to start informers", zap.Error(err))
	}

	logger.Info("Starting the knative activator")

	// Create the transport used by both the activator->QP probe and the proxy.
	// It's important that the throttler and the activatorhandler share this
	// transport so that throttler probe connections can be reused after probing
	// (via keep-alive) to send real requests, avoiding needing an extra
	// reconnect for the first request after the probe succeeds.
	logger.Debugf("MaxIdleProxyConns: %d, MaxIdleProxyConnsPerHost: %d", env.MaxIdleProxyConns, env.MaxIdleProxyConnsPerHost)
	transport := pkgnet.NewProxyAutoTransport(env.MaxIdleProxyConns, env.MaxIdleProxyConnsPerHost)

	// Fetch networking configuration to determine whether EnableMeshPodAddressability
	// is enabled or not.
	networkCM, err := kubeclient.Get(ctx).CoreV1().ConfigMaps(system.Namespace()).Get(ctx, netcfg.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		logger.Fatalw("Failed to fetch network config", zap.Error(err))
	}
	networkConfig, err := network.NewConfigFromConfigMap(networkCM)
	if err != nil {
		logger.Fatalw("Failed to construct network config", zap.Error(err))
	}

	// Enable TLS for connections to queue-proxy when system-internal-tls is enabled.
	tlsEnabled := networkConfig.SystemInternalTLSEnabled()

	var certCache *certificate.CertCache

	// Enable TLS client when queue-proxy-ca is specified.
	// At this moment activator with TLS does not disable HTTP.
	// See also https://github.com/knative/serving/issues/12808.
	if tlsEnabled {
		logger.Info("Knative system-internal-tls is enabled")
		certCache, err = certificate.NewCertCache(ctx)
		if err != nil {
			logger.Fatalw("Failed to create certificate cache", zap.Error(err))
		}
		transport = pkgnet.NewProxyAutoTLSTransport(env.MaxIdleProxyConns, env.MaxIdleProxyConnsPerHost, certCache.TLSContext())
	}

	// Start throttler.
	throttler := activatornet.NewThrottler(ctx, env.PodIP)
	go throttler.Run(ctx, transport, networkConfig.EnableMeshPodAddressability, networkConfig.MeshCompatibilityMode)

	// Set up our config store
	configMapWatcher := configmapinformer.NewInformedWatcher(kubeClient, system.Namespace())
	configStore := activatorconfig.NewStore(logger)
	configStore.WatchConfigs(configMapWatcher)

	statCh := make(chan []asmetrics.StatMessage)
	defer close(statCh)

	// Open a WebSocket connection to the autoscaler.
	autoscalerEndpoint := "ws://" + pkgnet.GetServiceHostname("autoscaler", system.Namespace()) + autoscalerPort
	logger.Info("Connecting to Autoscaler at ", autoscalerEndpoint)
	statSink := websocket.NewDurableSendingConnection(autoscalerEndpoint, logger)
	defer statSink.Shutdown()
	go activator.ReportStats(logger, statSink, statCh)

	// Create and run our concurrency reporter
	concurrencyReporter := activatorhandler.NewConcurrencyReporter(ctx, env.PodName, statCh, mp)
	go concurrencyReporter.Run(ctx.Done())

	// Create activation handler chain
	// Note: innermost handlers are specified first, ie. the last handler in the chain will be executed first
	ah := activatorhandler.New(ctx, throttler, transport, networkConfig.EnableMeshPodAddressability, logger, tlsEnabled, tp)
	ah = handler.NewTimeoutHandler(ah, "activator request timeout", func(r *http.Request) (time.Duration, time.Duration, time.Duration) {
		if rev := activatorhandler.RevisionFrom(r.Context()); rev != nil {
			responseStartTimeout := 0 * time.Second
			if rev.Spec.ResponseStartTimeoutSeconds != nil {
				responseStartTimeout = time.Duration(*rev.Spec.ResponseStartTimeoutSeconds) * time.Second
			}
			idleTimeout := 0 * time.Second
			if rev.Spec.IdleTimeoutSeconds != nil {
				idleTimeout = time.Duration(*rev.Spec.IdleTimeoutSeconds) * time.Second
			}
			return time.Duration(*rev.Spec.TimeoutSeconds) * time.Second, responseStartTimeout, idleTimeout
		}
		return apiconfig.DefaultRevisionTimeoutSeconds * time.Second,
			apiconfig.DefaultRevisionResponseStartTimeoutSeconds * time.Second,
			apiconfig.DefaultRevisionIdleTimeoutSeconds * time.Second
	})
	ah = concurrencyReporter.Handler(ah)
	ah = activatorhandler.NewTracingHandler(tp, ah)
	reqLogHandler, err := pkghttp.NewRequestLogHandler(ah, logging.NewSyncFileWriter(os.Stdout), "",
		requestLogTemplateInputGetter, false /*enableProbeRequestLog*/)
	if err != nil {
		logger.Fatalw("Unable to create request log handler", zap.Error(err))
	}
	ah = reqLogHandler

	// NOTE: MetricHandler is being used as the outermost handler of the meaty bits. We're not interested in measuring
	// the healthchecks or probes.
	ah = activatorhandler.NewMetricHandler(env.PodName, ah)
	// We need the context handler to run first so ctx gets the revision info.
	ah = activatorhandler.WrapActivatorHandlerWithFullDuplex(ah, logger)
	ah = activatorhandler.NewContextHandler(ctx, ah, configStore)

	// Network probe handlers.
	ah = &activatorhandler.ProbeHandler{NextHandler: ah}
	ah = netprobe.NewHandler(ah)
	// Set up our health check based on the health of stat sink and environmental factors.
	sigCtx := signals.NewContext()
	hc := newHealthCheck(sigCtx, logger, statSink)
	ah = &activatorhandler.HealthHandler{HealthCheck: hc, NextHandler: ah, Logger: logger}

	// Watch the logging config map and dynamically update logging levels.
	configMapWatcher.Watch(pkglogging.ConfigMapName(), pkglogging.UpdateLevelFromConfigMap(logger, atomicLevel, component))

	// Watch the observability config map
	configMapWatcher.Watch(o11yconfigmap.Name(),
		updateRequestLogFromConfigMap(logger, reqLogHandler),
		pprof.UpdateFromConfigMap)

	if err = configMapWatcher.Start(ctx.Done()); err != nil {
		logger.Fatalw("Failed to start configuration manager", zap.Error(err))
	}

	servers := map[string]*http.Server{
		"http1":   pkgnet.NewServer(":"+strconv.Itoa(networking.BackendHTTPPort), ah),
		"h2c":     pkgnet.NewServer(":"+strconv.Itoa(networking.BackendHTTP2Port), ah),
		"profile": pprof.Server,
	}

	errCh := make(chan error, len(servers))
	for name, server := range servers {
		go func(name string, s *http.Server) {
			// Don't forward ErrServerClosed as that indicates we're already shutting down.
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s server failed: %w", name, err)
			}
		}(name, server)
	}

	// Enable TLS server when system-internal-tls is specified.
	// At this moment activator with TLS does not disable HTTP.
	// See also https://github.com/knative/serving/issues/12808.
	if tlsEnabled {
		name, server := "https", pkgnet.NewServer(":"+strconv.Itoa(networking.BackendHTTPSPort), ah)
		go func(name string, s *http.Server) {
			s.TLSConfig = &tls.Config{
				MinVersion:     tls.VersionTLS13,
				GetCertificate: certCache.GetCertificate,
			}
			// Don't forward ErrServerClosed as that indicates we're already shutting down.
			if err := s.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s server failed: %w", name, err)
			}
		}(name, server)
	}

	// Wait for the signal to drain.
	select {
	case <-sigCtx.Done():
		logger.Info("Received SIGTERM")
	case err := <-errCh:
		logger.Errorw("Failed to run HTTP server", zap.Error(err))
	}

	// The drain has started (we are now failing readiness probes).  Let the effects of this
	// propagate so that new requests are no longer routed our way.
	logger.Infof("Sleeping %v to allow K8s propagation of non-ready state", pkgnet.DefaultDrainTimeout)
	time.Sleep(pkgnet.DefaultDrainTimeout)
	logger.Info("Done waiting, shutting down servers.")

	// Drain outstanding requests, and stop accepting new ones.
	for _, server := range servers {
		server.Shutdown(context.Background())
	}
	logger.Info("Servers shutdown.")
}

func newHealthCheck(sigCtx context.Context, logger *zap.SugaredLogger, statSink *websocket.ManagedConnection) func() error {
	once := sync.Once{}
	return func() error {
		select {
		// When we get SIGTERM (sigCtx done), let readiness probes start failing.
		case <-sigCtx.Done():
			once.Do(func() {
				logger.Info("Signal context canceled")
			})
			return errors.New("received SIGTERM from kubelet")
		default:
			logger.Debug("No signal yet.")
			return statSink.Status()
		}
	}
}

func flush(logger *zap.SugaredLogger) {
	logger.Sync()
	os.Stdout.Sync()
	os.Stderr.Sync()
}
