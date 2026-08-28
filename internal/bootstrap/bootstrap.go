// Package bootstrap holds the manager wiring. It is a package of its own rather
// than the body of main() so that the parts with decisions in them — flag
// parsing, scheme registration, how the manager is configured — can be tested
// directly, leaving main() as the handful of lines that genuinely cannot be.
package bootstrap

import (
	"context"
	"flag"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
	"github.com/sahilkalgutkar/tenant-operator/internal/controller"
)

// LeaderElectionID is the lock the manager contends on. Two managers
// reconciling the same Tenant would fight over every object they both own, so
// leader election is not optional for a real deployment — it is only turned off
// for local runs against a throwaway cluster.
const LeaderElectionID = "tenant-operator.tenancy.sahilkalgutkar.io"

// Options is everything the operator can be configured with.
type Options struct {
	MetricsAddr    string
	ProbeAddr      string
	LeaderElection bool
	WebhookPort    int
	CertDir        string
	EnableWebhooks bool
}

// DefaultOptions are the values I would want in a cluster, not the values that
// are most convenient locally: metrics and probes bound, leader election on,
// webhooks served.
func DefaultOptions() Options {
	return Options{
		MetricsAddr:    ":8080",
		ProbeAddr:      ":8081",
		LeaderElection: true,
		WebhookPort:    9443,
		CertDir:        "/tmp/k8s-webhook-server/serving-certs",
		EnableWebhooks: true,
	}
}

// ParseFlags turns command-line arguments into Options.
func ParseFlags(args []string, out io.Writer) (Options, error) {
	opts := DefaultOptions()

	fs := flag.NewFlagSet("tenant-operator", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&opts.MetricsAddr, "metrics-bind-address", opts.MetricsAddr, "address the metrics endpoint binds to")
	fs.StringVar(&opts.ProbeAddr, "health-probe-bind-address", opts.ProbeAddr, "address the health probe endpoint binds to")
	fs.BoolVar(&opts.LeaderElection, "leader-elect", opts.LeaderElection, "run leader election so only one manager reconciles at a time")
	fs.IntVar(&opts.WebhookPort, "webhook-port", opts.WebhookPort, "port the admission webhook server binds to")
	fs.StringVar(&opts.CertDir, "webhook-cert-dir", opts.CertDir, "directory holding the webhook server's tls.crt and tls.key")
	fs.BoolVar(&opts.EnableWebhooks, "enable-webhooks", opts.EnableWebhooks, "serve the defaulting and validating webhooks")

	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	return opts, opts.Validate()
}

// Validate rejects combinations that would start a manager that cannot work.
func (o Options) Validate() error {
	if o.EnableWebhooks && o.CertDir == "" {
		return fmt.Errorf("--webhook-cert-dir is required when webhooks are enabled")
	}
	if o.EnableWebhooks && (o.WebhookPort <= 0 || o.WebhookPort > 65535) {
		return fmt.Errorf("--webhook-port %d is not a valid port", o.WebhookPort)
	}
	if o.LeaderElection && o.ProbeAddr == "" {
		// A leader-elected manager that cannot be probed is one Kubernetes
		// cannot tell has lost its lease.
		return fmt.Errorf("--health-probe-bind-address is required when leader election is enabled")
	}
	return nil
}

// NewScheme registers everything the manager's client needs to decode.
func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(tenancyv1alpha1.AddToScheme(s))
	return s
}

// ManagerOptions maps my Options onto controller-runtime's.
func (o Options) ManagerOptions(scheme *runtime.Scheme) ctrl.Options {
	return ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: o.MetricsAddr},
		HealthProbeBindAddress: o.ProbeAddr,
		LeaderElection:         o.LeaderElection,
		LeaderElectionID:       LeaderElectionID,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    o.WebhookPort,
			CertDir: o.CertDir,
		}),
	}
}

// Setup attaches the controller, the webhooks and the probes to a manager.
// Taking a manager rather than building one means the envtest suite can run
// this exact wiring against a real API server.
func Setup(mgr ctrl.Manager, o Options) error {
	reconciler := &controller.TenantReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("tenant-controller"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the tenant controller: %w", err)
	}

	if o.EnableWebhooks {
		if err := tenancyv1alpha1.SetupTenantWebhookWithManager(mgr); err != nil {
			return fmt.Errorf("setting up the tenant webhooks: %w", err)
		}
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("adding the health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("adding the readiness check: %w", err)
	}
	return nil
}

// Run builds a manager from the options and blocks until the context is done.
func Run(ctx context.Context, o Options) error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("loading the kubeconfig: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, o.ManagerOptions(NewScheme()))
	if err != nil {
		return fmt.Errorf("starting the manager: %w", err)
	}
	if err := Setup(mgr, o); err != nil {
		return err
	}
	return mgr.Start(ctx)
}
