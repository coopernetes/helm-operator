package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/weaveworks/flux/checkpoint"
	clientset "github.com/weaveworks/flux/integrations/client/clientset/versioned"
	ifinformers "github.com/weaveworks/flux/integrations/client/informers/externalversions"
	fluxhelm "github.com/weaveworks/flux/integrations/helm"
	"github.com/weaveworks/flux/integrations/helm/chartsync"
	"github.com/weaveworks/flux/integrations/helm/operator"
	"github.com/weaveworks/flux/integrations/helm/release"
	"github.com/weaveworks/flux/integrations/helm/status"
)

var (
	fs     *pflag.FlagSet
	err    error
	logger log.Logger

	versionFlag *bool

	kubeconfig *string
	master     *string

	tillerIP        *string
	tillerPort      *string
	tillerNamespace *string

	tillerTLSVerify   *bool
	tillerTLSEnable   *bool
	tillerTLSKey      *string
	tillerTLSCert     *string
	tillerTLSCACert   *string
	tillerTLSHostname *string

	chartsSyncInterval *time.Duration
	logReleaseDiffs    *bool
	updateDependencies *bool

	gitPollInterval *time.Duration
	gitTimeout      *time.Duration

	listenAddr *string
)

const (
	product            = "weave-flux-helm"
	ErrOperatorFailure = "Operator failure: %q"
)

var version = "unversioned"

func init() {
	// Flags processing
	fs = pflag.NewFlagSet("default", pflag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "DESCRIPTION\n")
		fmt.Fprintf(os.Stderr, "  helm-operator releases Helm charts from git.\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "FLAGS\n")
		fs.PrintDefaults()
	}

	versionFlag = fs.Bool("version", false, "print version and exit")

	kubeconfig = fs.String("kubeconfig", "", "path to a kubeconfig; required if out-of-cluster")
	master = fs.String("master", "", "address of the Kubernetes API server; overrides any value in kubeconfig; required if out-of-cluster")

	tillerIP = fs.String("tiller-ip", "", "Tiller IP address; required if run out-of-cluster")
	tillerPort = fs.String("tiller-port", "", "Tiller port; required if run out-of-cluster")
	tillerNamespace = fs.String("tiller-namespace", "kube-system", "Tiller namespace")

	tillerTLSVerify = fs.Bool("tiller-tls-verify", false, "verify TLS certificate from Tiller; will enable TLS communication when provided")
	tillerTLSEnable = fs.Bool("tiller-tls-enable", false, "enable TLS communication with Tiller; if provided, requires TLSKey and TLSCert to be provided as well")
	tillerTLSKey = fs.String("tiller-tls-key-path", "/etc/fluxd/helm/tls.key", "path to private key file used to communicate with the Tiller server")
	tillerTLSCert = fs.String("tiller-tls-cert-path", "/etc/fluxd/helm/tls.crt", "path to certificate file used to communicate with the Tiller server")
	tillerTLSCACert = fs.String("tiller-tls-ca-cert-path", "", "path to CA certificate file used to validate the Tiller server; required if tiller-tls-verify is enabled")
	tillerTLSHostname = fs.String("tiller-tls-hostname", "", "server name used to verify the hostname on the returned certificates from the server")

	chartsSyncInterval = fs.Duration("charts-sync-interval", 3*time.Minute, "period on which to reconcile the Helm releases with FluxHelmRelease resources")
	logReleaseDiffs = fs.Bool("log-release-diffs", false, "log the diff when a chart release diverges; potentially insecure")
	updateDependencies = fs.Bool("update-chart-deps", true, "Update chart dependencies before installing/upgrading a release")

	gitPollInterval = fs.Duration("git-poll-interval", 5*time.Minute, "period on which to poll for changes to the git repo")
	gitTimeout = fs.Duration("git-timeout", 20*time.Second, "duration after which git operations time out")
}

func main() {
	// Stop glog complaining
	flag.CommandLine.Parse([]string{"-logtostderr"})
	// Now do our own
	fs.Parse(os.Args)

	if *versionFlag {
		println(version)
		os.Exit(0)
	}

	// LOGGING ------------------------------------------------------------------------------
	{
		logger = log.NewLogfmtLogger(os.Stderr)
		logger = log.With(logger, "ts", log.DefaultTimestampUTC)
		logger = log.With(logger, "caller", log.DefaultCaller)
	}

	// SHUTDOWN  ----------------------------------------------------------------------------
	errc := make(chan error)

	// Shutdown trigger for goroutines
	shutdown := make(chan struct{})
	shutdownWg := &sync.WaitGroup{}

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		errc <- fmt.Errorf("%s", <-c)
	}()

	defer func() {
		logger.Log("exiting...", <-errc)
		close(shutdown)
		shutdownWg.Wait()
	}()

	mainLogger := log.With(logger, "component", "helm-operator")

	// CLUSTER ACCESS -----------------------------------------------------------------------
	cfg, err := clientcmd.BuildConfigFromFlags(*master, *kubeconfig)
	if err != nil {
		mainLogger.Log("error", fmt.Sprintf("Error building kubeconfig: %v", err))
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		mainLogger.Log("error", fmt.Sprintf("Error building kubernetes clientset: %v", err))
		os.Exit(1)
	}

	// CUSTOM RESOURCES CLIENT --------------------------------------------------------------
	ifClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		mainLogger.Log("error", fmt.Sprintf("Error building integrations clientset: %v", err))
		//errc <- fmt.Errorf("Error building integrations clientset: %v", err)
		os.Exit(1)
	}

	// HELM ---------------------------------------------------------------------------------
	helmClient := fluxhelm.ClientSetup(log.With(logger, "component", "helm"), kubeClient, fluxhelm.TillerOptions{
		Host:        *tillerIP,
		Port:        *tillerPort,
		Namespace:   *tillerNamespace,
		TLSVerify:   *tillerTLSVerify,
		TLSEnable:   *tillerTLSEnable,
		TLSKey:      *tillerTLSKey,
		TLSCert:     *tillerTLSCert,
		TLSCACert:   *tillerTLSCACert,
		TLSHostname: *tillerTLSHostname,
	})

	// The status updater, to keep track the release status for each
	// FluxHelmRelease. It runs as a separate loop for now.
	statusUpdater := status.New(ifClient, kubeClient, helmClient)
	go statusUpdater.Loop(shutdown, log.With(logger, "component", "annotator"))

	// release instance is needed during the sync of Charts changes and during the sync of FluxHelmRelease changes
	rel := release.New(log.With(logger, "component", "release"), helmClient)
	// CHARTS CHANGES SYNC ------------------------------------------------------------------
	chartSync := chartsync.New(log.With(logger, "component", "chartsync"),
		chartsync.Polling{Interval: *chartsSyncInterval},
		chartsync.Clients{KubeClient: *kubeClient, IfClient: *ifClient},
		rel, chartsync.Config{LogDiffs: *logReleaseDiffs, UpdateDeps: *updateDependencies})
	chartSync.Run(shutdown, errc, shutdownWg)

	// OPERATOR - CUSTOM RESOURCE CHANGE SYNC -----------------------------------------------
	// CUSTOM RESOURCES CACHING SETUP -------------------------------------------------------
	//				SharedInformerFactory sets up informer, that maps resource type to a cache shared informer.
	//				operator attaches event handler to the informer and syncs the informer cache
	ifInformerFactory := ifinformers.NewSharedInformerFactory(ifClient, 30*time.Second)
	// Reference to shared index informers for the FluxHelmRelease
	fhrInformer := ifInformerFactory.Flux().V1beta1().FluxHelmReleases()

	opr := operator.New(log.With(logger, "component", "operator"), *logReleaseDiffs, kubeClient, fhrInformer, chartSync)
	// Starts handling k8s events related to the given resource kind
	go ifInformerFactory.Start(shutdown)

	checkpoint.CheckForUpdates(product, version, nil, log.With(logger, "component", "checkpoint"))

	if err = opr.Run(1, shutdown, shutdownWg); err != nil {
		msg := fmt.Sprintf("Failure to run controller: %s", err.Error())
		logger.Log("error", msg)
		errc <- fmt.Errorf(ErrOperatorFailure, err)
	}
}
