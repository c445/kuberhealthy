// Copyright 2018 Comcast Cable Communications Management, LLC
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Kuberhealthy is an enhanced health check for Kubernetes clusters.
package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/integrii/flaggy"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/Comcast/kuberhealthy/v2/pkg/khcheckcrd"
	"github.com/Comcast/kuberhealthy/v2/pkg/khstatecrd"
	"github.com/Comcast/kuberhealthy/v2/pkg/kubeClient"
	"github.com/Comcast/kuberhealthy/v2/pkg/masterCalculation"
)

// status represents the current Kuberhealthy OK:Error state
var kubeConfigFile = filepath.Join(os.Getenv("HOME"), ".kube", "config")
var listenAddress = ":8080"
var podNamespace = os.Getenv("POD_NAMESPACE")
var isMaster bool                  // indicates this instance is the master and should be running checks
var upcomingMasterState bool       // the upcoming master state on next interval
var lastMasterChangeTime time.Time // indicates the last time a master change was seen
var listenNamespace string         // namespace to listen (watch/get) `khcheck` resources on.  If blank, all namespaces will be monitored.

var terminationGracePeriod = time.Minute * 5 // keep calibrated with kubernetes terminationGracePeriodSeconds

// flags indicating that checks of specific types should be used
var enableForceMaster bool // force master mode - for debugging
var enableDebug bool       // enable debug logging
var logLevel = "info"

// the hostname of this pod
var podHostname string

// external check configs
const KHExternalReportingURL = "KH_EXTERNAL_REPORTING_URL"

// default run interval set by kuberhealthy
const DefaultRunInterval = time.Minute * 10

// the key used in the annotation that holds the check's short name
const KH_CHECK_NAME_ANNOTATION_KEY = "comcast.github.io/check-name"

var externalCheckReportingURL = os.Getenv(KHExternalReportingURL)

// InfluxDB connection configuration
var enableInflux = false
var influxURL = ""
var influxUsername = ""
var influxPassword = ""
var influxDB = "http://localhost:8086"
var kuberhealthy *Kuberhealthy

var khStateClient *khstatecrd.KuberhealthyStateClient

// constants for using the kuberhealthy status CRD
const stateCRDGroup = "comcast.github.io"
const stateCRDVersion = "v1"
const stateCRDResource = "khstates"

var khCheckClient *khcheckcrd.KuberhealthyCheckClient

// constants for using the kuberhealthy check CRD
const checkCRDGroup = "comcast.github.io"
const checkCRDVersion = "v1"
const checkCRDResource = "khchecks"

// the global kubernetes client
var kubernetesClient *kubernetes.Clientset

func init() {

	// setup flaggy
	flaggy.SetDescription("Kuberhealthy is an in-cluster synthetic health checker for Kubernetes.")
	flaggy.String(&kubeConfigFile, "", "kubecfg", "(optional) absolute path to the kubeconfig file")
	flaggy.String(&listenAddress, "l", "listenAddress", "The port for kuberhealthy to listen on for web requests")
	flaggy.Bool(&enableForceMaster, "", "forceMaster", "Set to true to enable local testing, forced master mode.")
	flaggy.Bool(&enableDebug, "d", "debug", "Set to true to enable debug.")
	flaggy.String(&logLevel, "", "log-level", fmt.Sprintf("Log level to be used one of [%s].", getAllLogLevel()))
	flaggy.String(&listenNamespace, "", "listenNamespace", "Kuberhealthy will only monitor khcheck resources from this namespace, if specified.")
	// Influx flags
	flaggy.String(&influxUsername, "", "influxUser", "Username for the InfluxDB instance")
	flaggy.String(&influxPassword, "", "influxPassword", "Password for the InfluxDB instance")
	flaggy.String(&influxURL, "", "influxUrl", "Address for the InfluxDB instance")
	flaggy.String(&influxDB, "", "influxDB", "Name of the InfluxDB database")
	flaggy.Bool(&enableInflux, "", "enableInflux", "Set to true to enable metric forwarding to Influx DB.")
	flaggy.Parse()

	parsedLogLevel, err := log.ParseLevel(logLevel)
	if err != nil {
		log.Fatalln("Unable to parse log-level flag: ", err)
	}

	// log to stdout and set the level to info by default
	log.SetOutput(os.Stdout)
	log.SetLevel(parsedLogLevel)
	log.Infoln("Startup Arguments:", os.Args)

	// parse external check URL configuration
	if len(externalCheckReportingURL) == 0 {
		if len(podNamespace) == 0 {
			log.Fatalln("KH_EXTERNAL_REPORTING_URL environment variable not set and POD_NAMESPACE environment variable was blank.  Could not determine Kuberhealthy callback URL.")
		}
		externalCheckReportingURL = "http://kuberhealthy." + podNamespace + ".svc.cluster.local/externalCheckStatus"
	}
	log.Infoln("External check reporting URL set to:", externalCheckReportingURL)

	// handle debug logging
	debugEnv := os.Getenv("DEBUG")
	if len(debugEnv) > 0 {
		enableDebug, err = strconv.ParseBool(debugEnv)
		if err != nil {
			log.Warningln("Failed to parse bool for DEBUG setting:", err)
		}
	}
	if enableDebug {
		log.Infoln("Enabling debug logging")
		log.SetLevel(log.DebugLevel)
		masterCalculation.EnableDebug()

		// enable debug on klog for dependencies
		klog.V(10)
	}

	// Handle force master mode
	if enableForceMaster {
		log.Infoln("Enabling forced master mode")
		masterCalculation.DebugAlwaysMasterOn()
	}

	// determine the name of this pod from the POD_NAME environment variable
	podHostname, err = getEnvVar("POD_NAME")
	if err != nil {
		log.Fatalln("Failed to determine my hostname!")
	}

	// setup all clients
	err = initKubernetesClients()
	if err != nil {
		log.Fatalln("Failed to bootstrap kubernetes clients:", err)
	}
}

func getGID() uint64 {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	b = b[:bytes.IndexByte(b, ' ')]
	n, _ := strconv.ParseUint(string(b), 10, 64)
	return n
}

type goroutineFormatter struct {
	t log.Formatter
}

func (g goroutineFormatter) Format(e *log.Entry) ([]byte, error) {
	f, err := g.t.Format(e)
	if err != nil {
		return f, err
	}
	return []byte(fmt.Sprintf("%d: %s", getGID(), f)), nil
}


func main() {
	log.Info("Starting pprof endpoint on :6060")
	log.SetFormatter(goroutineFormatter{t: new(log.TextFormatter)})
	pprofMux := http.NewServeMux()
	pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
	pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	server := &http.Server{Addr: ":6060", Handler: pprofMux}
	go func() {
		if err := server.ListenAndServe(); err != nil {
			log.Error(err, "serving pprof endpoint failed")
			os.Exit(1)
		}
	}()

	// Create a new Kuberhealthy struct
	kuberhealthy = NewKuberhealthy()
	kuberhealthy.ListenAddr = listenAddress

	// create run context and start listening for shutdown interrupts
	khRunCtx, khRunCtxCancelFunc := context.WithCancel(context.Background())
	kuberhealthy.shutdownCtxFunc = khRunCtxCancelFunc // load the KH struct with a func to shutdown its control system
	go listenForInterrupts()

	// tell Kuberhealthy to start all checks and master change monitoring
	kuberhealthy.Start(khRunCtx)

	time.Sleep(time.Second * 90) // give the interrupt handler a period of time to call exit before we shutdown
	<-time.After(terminationGracePeriod + (time.Second * 10))
	log.Errorln("shutdown: main loop was ready for shutdown for too long. exiting.")
	os.Exit(1)
}

// listenForInterrupts watches for termination signals and acts on them
func listenForInterrupts() {
	// shutdown signal handling
	sigChan := make(chan os.Signal, 1)

	// register for shutdown events on sigChan
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGKILL)
	log.Infoln("shutdown: waiting for sigChan notification...")
	<-sigChan
	log.Infoln("shutdown: Shutting down due to sigChan signal...")

	// wait for check to fully shutdown before exiting
	doneChan := make(chan struct{})
	go kuberhealthy.Shutdown(doneChan)

	// wait for checks to be done shutting down before exiting
	select {
	case <-doneChan:
		log.Infoln("shutdown: Shutdown gracefully completed!")
		log.Infoln("shutdown: exiting 0")
		os.Exit(0)
	case <-sigChan:
		log.Warningln("shutdown: Shutdown forced from multiple interrupts!")
		log.Infoln("shutdown: exiting 1")
		os.Exit(1)
	case <-time.After(terminationGracePeriod):
		log.Errorln("shutdown: Shutdown took too long.  Shutting down forcefully!")
		log.Infoln("shutdown: exiting 1")
		os.Exit(1)
	}
}

// determineCheckStateFromEnvVar determines a check's enabled state based on
// the supplied environment variable
func determineCheckStateFromEnvVar(envVarName string) bool {
	enabledState, err := strconv.ParseBool(os.Getenv(envVarName))
	if err != nil {
		log.Debugln("Had an error parsing the environment variable", envVarName, err)
		return true // by default, the check is on
	}
	return enabledState
}

// initKubernetesClients creates the appropriate CRD clients and kubernetes client to be used in all cases. Issue #181
func initKubernetesClients() error {

	// make a new kuberhealthy client
	kc, err := kubeClient.Create(kubeConfigFile)
	if err != nil {
		return err
	}
	kubernetesClient = kc

	// make a new crd check client
	checkClient, err := khcheckcrd.Client(checkCRDGroup, checkCRDVersion, kubeConfigFile, listenNamespace)
	if err != nil {
		return err
	}
	khCheckClient = checkClient

	// make a new crd state client
	stateClient, err := khstatecrd.Client(stateCRDGroup, stateCRDVersion, kubeConfigFile, listenNamespace)
	if err != nil {
		return err
	}
	khStateClient = stateClient

	return nil
}
