/*
Copyright 2018 Planet Labs Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing permissions
and limitations under the License.
*/

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"gopkg.in/alecthomas/kingpin.v2"
	client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog"

	"github.com/planetlabs/draino/internal/aws"
	"github.com/planetlabs/draino/internal/kubernetes"
	"github.com/planetlabs/draino/internal/metrics"
)

// Default leader election settings.
const (
	DefaultLeaderElectionLeaseDuration time.Duration = 15 * time.Second
	DefaultLeaderElectionRenewDeadline time.Duration = 10 * time.Second
	DefaultLeaderElectionRetryPeriod   time.Duration = 2 * time.Second
)

func init() {
	// Initialize the counters with label values
	initializeCounter(metrics.NodesCordoned, "succeeded")
	initializeCounter(metrics.NodesCordoned, "failed")
	initializeCounter(metrics.NodesUncordoned, "succeeded")
	initializeCounter(metrics.NodesUncordoned, "failed")
	initializeCounter(metrics.NodesDrained, "succeeded")
	initializeCounter(metrics.NodesDrained, "failed")
	initializeCounter(metrics.NodesDrainScheduled, "succeeded")
	initializeCounter(metrics.NodesDrainScheduled, "failed")
}

func initializeCounter(counter *prometheus.CounterVec, labelValue string) {
	counter.WithLabelValues(labelValue).Add(0)
}

func main() {
	var (
		app = kingpin.New(filepath.Base(os.Args[0]), "Automatically cordons and drains nodes that match the supplied conditions.").DefaultEnvars()

		debug            = app.Flag("debug", "Run with debug logging.").Short('d').Bool()
		listen           = app.Flag("listen", "Address at which to expose /metrics and /healthz.").Default(":10002").String()
		kubecfg          = app.Flag("kubeconfig", "Path to kubeconfig file. Leave unset to use in-cluster config.").String()
		apiserver        = app.Flag("master", "Address of Kubernetes API server. Leave unset to use in-cluster config.").String()
		dryRun           = app.Flag("dry-run", "Emit an event without cordoning or draining matching nodes.").Bool()
		maxGracePeriod   = app.Flag("max-grace-period", "Maximum time evicted pods will be given to terminate gracefully.").Default(kubernetes.DefaultMaxGracePeriod.String()).Duration()
		evictionHeadroom = app.Flag("eviction-headroom", "Additional time to wait after a pod's termination grace period for it to have been deleted.").Default(kubernetes.DefaultEvictionOverhead.String()).Duration()
		drainBuffer      = app.Flag("drain-buffer", "Minimum time between starting each drain. Nodes are always cordoned immediately.").Default(kubernetes.DefaultDrainBuffer.String()).Duration()
		nodeLabels       = app.Flag("node-label", "(Deprecated) Nodes with this label will be eligible for cordoning and draining. May be specified multiple times").Strings()
		nodeLabelsExpr   = app.Flag("node-label-expr", "Nodes that match this expression will be eligible for cordoning and draining.").String()
		namespace        = app.Flag("namespace", "Namespace used to create leader election lock object.").Default("kube-system").String()

		leaderElectionLeaseDuration = app.Flag("leader-election-lease-duration", "Lease duration for leader election.").Default(DefaultLeaderElectionLeaseDuration.String()).Duration()
		leaderElectionRenewDeadline = app.Flag("leader-election-renew-deadline", "Leader election renew deadline.").Default(DefaultLeaderElectionRenewDeadline.String()).Duration()
		leaderElectionRetryPeriod   = app.Flag("leader-election-retry-period", "Leader election retry period.").Default(DefaultLeaderElectionRetryPeriod.String()).Duration()
		leaderElectionTokenName     = app.Flag("leader-election-token-name", "Leader election token name.").Default(kubernetes.Component).String()

		skipDrain             = app.Flag("skip-drain", "Whether to skip draining nodes after cordoning.").Default("false").Bool()
		evictDaemonSetPods    = app.Flag("evict-daemonset-pods", "Evict pods that were created by an extant DaemonSet.").Bool()
		evictStatefulSetPods  = app.Flag("evict-statefulset-pods", "Evict pods that were created by an extant StatefulSet.").Bool()
		evictLocalStoragePods = app.Flag("evict-emptydir-pods", "Evict pods with local storage, i.e. with emptyDir volumes.").Bool()
		evictUnreplicatedPods = app.Flag("evict-unreplicated-pods", "Evict pods that were not created by a replication controller.").Bool()
		allowForceDelete      = app.Flag("allow-force-delete", "Allow force to delete the pods if they do not terminate within the grace period.").Bool()

		protectedPodAnnotations      = app.Flag("protected-pod-annotation", "Protect pods with this annotation from eviction. May be specified multiple times.").PlaceHolder("KEY[=VALUE]").Strings()
		ingoreSafeToEvictAnnotations = app.Flag("ignore-safe-to-evict-annotation", "Ignore the cluster-autoscaler.kubernetes.io/safe-to-evict=false annotation.").Bool()

		awsRegion              = app.Flag("aws-region", "AWS region to use for AWS API calls.").String()
		awsSetUnhealthyOnDrain = app.Flag("aws-set-unhealthy-on-drain", "Marks AWS ASG instances as unhealthy after node drained").Default("false").Bool()

		conditions = app.Arg("node-conditions", "Nodes for which any of these conditions are true will be cordoned and drained.").Required().Strings()
	)
	kingpin.MustParse(app.Parse(os.Args[1:]))

	// this is required to make all packages using klog write to stderr instead of tmp files
	klog.InitFlags(nil)

	web := &httpRunner{l: *listen, h: map[string]http.Handler{
		"/metrics": promhttp.Handler(),
		"/healthz": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.Body.Close() }), // nolint:errcheck
	}}

	log, err := zap.NewProduction()
	if *debug {
		log, err = zap.NewDevelopment()
	}
	kingpin.FatalIfError(err, "cannot create log")
	defer log.Sync() // nolint:errcheck

	go func() {
		log.Info("web server is running", zap.String("listen", *listen))
		kingpin.FatalIfError(await(web), "error serving")
	}()

	// Create an ASGManager if awsSetUnhealthyOnDrain is true
	var asgManager *aws.ASGManager
	if *awsSetUnhealthyOnDrain {
		log.Info("Initializing AWS ASG Manager to mark instances as unhealthy.")
		asgManager = aws.NewASGManager(log, *awsRegion)
	}

	c, err := kubernetes.BuildConfigFromFlags(*apiserver, *kubecfg)
	kingpin.FatalIfError(err, "cannot create Kubernetes client configuration")

	cs, err := client.NewForConfig(c)
	kingpin.FatalIfError(err, "cannot create Kubernetes client")

	pf := []kubernetes.PodFilterFunc{kubernetes.MirrorPodFilter}
	if !*evictLocalStoragePods {
		pf = append(pf, kubernetes.LocalStoragePodFilter)
	}
	if !*evictUnreplicatedPods {
		pf = append(pf, kubernetes.UnreplicatedPodFilter)
	}
	if !*evictDaemonSetPods {
		pf = append(pf, kubernetes.NewDaemonSetPodFilter(cs))
	}
	if !*evictStatefulSetPods {
		pf = append(pf, kubernetes.NewStatefulSetPodFilter(cs))
	}
	systemKnownAnnotations := []string{}
	if !*ingoreSafeToEvictAnnotations {
		systemKnownAnnotations = append(systemKnownAnnotations, "cluster-autoscaler.kubernetes.io/safe-to-evict=false")
	}
	pf = append(pf, kubernetes.UnprotectedPodFilter(append(systemKnownAnnotations, *protectedPodAnnotations...)...))
	var h cache.ResourceEventHandler = kubernetes.NewDrainingResourceEventHandler(
		kubernetes.NewAPICordonDrainer(cs,
			kubernetes.MaxGracePeriod(*maxGracePeriod),
			kubernetes.EvictionHeadroom(*evictionHeadroom),
			kubernetes.WithSkipDrain(*skipDrain),
			kubernetes.WithPodFilter(kubernetes.NewPodFilters(pf...)),
			kubernetes.WithAPICordonDrainerLogger(log),
			kubernetes.WithAllowForceDelete(*allowForceDelete),
			kubernetes.WithASGManager(asgManager), // Pass the ASGManager if initialized
		),
		kubernetes.NewEventRecorder(cs),
		kubernetes.WithLogger(log),
		kubernetes.WithDrainBuffer(*drainBuffer),
		kubernetes.WithConditionsFilter(*conditions))

	if *dryRun {
		h = cache.FilteringResourceEventHandler{
			FilterFunc: kubernetes.NewNodeProcessed().Filter,
			Handler: kubernetes.NewDrainingResourceEventHandler(
				&kubernetes.NoopCordonDrainer{},
				kubernetes.NewEventRecorder(cs),
				kubernetes.WithLogger(log),
				kubernetes.WithDrainBuffer(*drainBuffer),
				kubernetes.WithConditionsFilter(*conditions)),
		}
	}

	if len(*nodeLabels) > 0 {
		log.Debug("node labels", zap.Any("labels", nodeLabels))
		if *nodeLabelsExpr != "" {
			kingpin.Fatalf("nodeLabels and NodeLabelsExpr cannot both be set")
		}
		if nodeLabelsExpr, err = kubernetes.ConvertLabelsToFilterExpr(*nodeLabels); err != nil {
			kingpin.Fatalf(err.Error())
		}
	}

	var nodeLabelFilter cache.ResourceEventHandler
	log.Debug("label expression", zap.Any("expr", nodeLabelsExpr))

	nodeLabelFilterFunc, err := kubernetes.NewNodeLabelFilter(nodeLabelsExpr, log)
	if err != nil {
		log.Sugar().Fatalf("Failed to parse node label expression: %v", err)
	}

	nodeLabelFilter = cache.FilteringResourceEventHandler{FilterFunc: nodeLabelFilterFunc, Handler: h}

	nodes := kubernetes.NewNodeWatch(cs, nodeLabelFilter)

	id, err := os.Hostname()
	kingpin.FatalIfError(err, "cannot get hostname")

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lock, err := resourcelock.New(
		"leases",
		*namespace,
		*leaderElectionTokenName,
		cs.CoreV1(),
		cs.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: kubernetes.NewEventRecorder(cs),
		},
	)
	kingpin.FatalIfError(err, "cannot create lock")

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: *leaderElectionLeaseDuration,
		RenewDeadline: *leaderElectionRenewDeadline,
		RetryPeriod:   *leaderElectionRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.Info("node watcher is running")
				kingpin.FatalIfError(await(nodes), "error watching")
			},
			OnStoppedLeading: func() {
				kingpin.Fatalf("lost leader election")
			},
		},
	})
}

type runner interface {
	Run(stop <-chan struct{})
}

func await(rs ...runner) error {
	stop := make(chan struct{})
	g := &run.Group{}
	for i := range rs {
		r := rs[i] // https://golang.org/doc/faq#closures_and_goroutines
		g.Add(func() error { r.Run(stop); return nil }, func(err error) { close(stop) })
	}
	return g.Run()
}

type httpRunner struct {
	l string
	h map[string]http.Handler
}

func (r *httpRunner) Run(stop <-chan struct{}) {
	rt := httprouter.New()
	for path, handler := range r.h {
		rt.Handler("GET", path, handler)
	}

	s := &http.Server{Addr: r.l, Handler: rt}
	ctx, cancel := context.WithTimeout(context.Background(), 0*time.Second)
	go func() {
		<-stop
		s.Shutdown(ctx) // nolint:errcheck
	}()
	s.ListenAndServe() // nolint:errcheck
	cancel()
}
