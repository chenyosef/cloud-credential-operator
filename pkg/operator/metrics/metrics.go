package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	credreqv1 "github.com/openshift/cloud-credential-operator/pkg/apis/cloudcredential/v1"
	"github.com/openshift/cloud-credential-operator/pkg/operator/credentialsrequest/constants"
	"github.com/openshift/cloud-credential-operator/pkg/operator/utils"
)

const (
	controllerName = "metrics"
)

var (
	metricCredentialsRequestTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cco_credentials_requests",
		Help: "Total number of credentials requests.",
	}, []string{"cloud_type"})

	// Capture the various conditions set on the CredentialsRequests
	metricCredentialsRequestConditions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cco_credentials_requests_conditions",
		Help: "Credentials requests with asserted conditions.",
	}, []string{"condition"})

	// MetricControllerReconcileTime tracks the length of time our reconcile loops take. controller-runtime
	// technically tracks this for us, but due to bugs currently also includes time in the queue, which leads to
	// extremely strange results. For now, track our own metric.
	MetricControllerReconcileTime = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cco_controller_reconcile_seconds",
			Help:    "Distribution of the length of time each controllers reconcile loop takes.",
			Buckets: []float64{0.001, 0.01, 0.1, 1, 10, 30, 60, 120},
		},
		[]string{"controller"},
	)
)

func init() {
	metrics.Registry.MustRegister(metricCredentialsRequestTotal)
	metrics.Registry.MustRegister(metricCredentialsRequestConditions)

	metrics.Registry.MustRegister(MetricControllerReconcileTime)
}

// Add creates a new metrics Calculator and adds it to the Manager.
func Add(mgr manager.Manager, kubeConfig string) error {
	mc := &Calculator{
		Client:   mgr.GetClient(),
		Interval: 2 * time.Minute,
	}
	err := mgr.Add(mc)
	if err != nil {
		return err
	}
	return nil
}

// Calculator runs in a goroutine and periodically calculates and publishes
// Prometheus metrics which will be exposed at our /metrics endpoint. Note that this is not
// a standard controller watching Kube resources, it runs periodically and then goes to sleep.
//
// This should be used for metrics which do not fit well into controller reconcile loops,
// things that are calculated globally rather than metrics releated to specific reconciliations.
type Calculator struct {
	Client client.Client

	// Interval is the length of time we sleep between metrics calculations.
	Interval time.Duration
}

// Start begins the metrics calculation loop.
func (mc *Calculator) Start(stopCh <-chan struct{}) error {
	log.Info("started metrics calculator goroutine")

	// Run forever, sleep at the end:
	wait.Until(func() {
		start := time.Now()
		mcLog := log.WithField("controller", controllerName)
		defer func() {
			dur := time.Since(start)
			MetricControllerReconcileTime.WithLabelValues(controllerName).Observe(dur.Seconds())
			mcLog.WithField("elapsed", dur).Info("reconcile complete")
		}()

		mcLog.Info("calculating metrics for all CredentialsRequests")

		ccoDisabled, err := utils.IsOperatorDisabled(mc.Client, mcLog)
		if err != nil {
			mcLog.WithError(err).Error("failed to determine whether CCO is disabled")
			return
		}

		credRequests := &credreqv1.CredentialsRequestList{}
		if err := mc.Client.List(context.TODO(), credRequests); err != nil {
			mcLog.WithError(err).Error("error listing CredentialsRequests")
			return
		}

		accumulator := newAccumulator(mcLog)
		for _, cr := range credRequests.Items {
			accumulator.processCR(&cr, ccoDisabled)
		}

		accumulator.setMetrics()
	}, mc.Interval, stopCh)

	return nil
}

func cloudProviderSpecToMetricsKey(cloud string) string {
	switch cloud {
	case "AWSProviderSpec":
		return "aws"
	case "AzureProviderSpec":
		return "azure"
	case "GCPProviderSpec":
		return "gcp"
	case "OpenStackProviderSpec":
		return "openstack"
	default:
		return "unknown"
	}
}

type credRequestAccumulator struct {
	logger log.FieldLogger

	crTotals     map[string]int
	crConditions map[credreqv1.CredentialsRequestConditionType]int
}

func newAccumulator(logger log.FieldLogger) *credRequestAccumulator {
	acc := &credRequestAccumulator{
		logger:       logger,
		crTotals:     map[string]int{},
		crConditions: map[credreqv1.CredentialsRequestConditionType]int{},
	}

	// make entries with '0' so we make sure to send updated metrics for any
	// condititons that may have cleared
	for _, c := range constants.FailureConditionTypes {
		acc.crConditions[c] = 0
	}

	return acc
}

func (a *credRequestAccumulator) processCR(cr *credreqv1.CredentialsRequest, ccoDisabled bool) {
	cloudType, err := utils.GetCredentialsRequestCloudType(cr.Spec.ProviderSpec)
	if err != nil {
		a.logger.WithError(err).Warningf("unable to determine cloud type for CredentialsRequest: %v", cr.Name)
	}
	cloudKey := cloudProviderSpecToMetricsKey(cloudType)
	a.crTotals[cloudKey]++

	// Skip reporting conditions if CCO is disabled, as we shouldn't be alerting in that case.
	if !ccoDisabled {
		for _, cond := range cr.Status.Conditions {
			if cond.Status == corev1.ConditionTrue {
				a.crConditions[cond.Type]++
			}
		}
	}
}

func (a *credRequestAccumulator) setMetrics() {
	for k, v := range a.crTotals {
		metricCredentialsRequestTotal.WithLabelValues(k).Set(float64(v))
	}

	for k, v := range a.crConditions {
		metricCredentialsRequestConditions.WithLabelValues(string(k)).Set(float64(v))
	}
}
