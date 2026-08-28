package bootstrap

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
)

func TestDefaultOptionsAreTheOnesIWantInACluster(t *testing.T) {
	opts := DefaultOptions()
	assert.True(t, opts.LeaderElection, "leader election should be on unless someone turns it off")
	assert.True(t, opts.EnableWebhooks)
	assert.NotEmpty(t, opts.MetricsAddr)
	assert.NotEmpty(t, opts.ProbeAddr)
	assert.NoError(t, opts.Validate())
}

func TestParseFlags(t *testing.T) {
	t.Run("no arguments gives the defaults", func(t *testing.T) {
		got, err := ParseFlags(nil, io.Discard)
		require.NoError(t, err)
		assert.Equal(t, DefaultOptions(), got)
	})

	t.Run("a local run can turn the cluster-only pieces off", func(t *testing.T) {
		got, err := ParseFlags([]string{
			"--leader-elect=false",
			"--enable-webhooks=false",
			"--metrics-bind-address=:9000",
			"--health-probe-bind-address=:9001",
		}, io.Discard)
		require.NoError(t, err)
		assert.False(t, got.LeaderElection)
		assert.False(t, got.EnableWebhooks)
		assert.Equal(t, ":9000", got.MetricsAddr)
		assert.Equal(t, ":9001", got.ProbeAddr)
	})

	t.Run("an unknown flag is an error, not a warning", func(t *testing.T) {
		_, err := ParseFlags([]string{"--reconcile-harder"}, io.Discard)
		assert.Error(t, err)
	})

	t.Run("flags that would start a broken manager are rejected", func(t *testing.T) {
		_, err := ParseFlags([]string{"--webhook-cert-dir="}, io.Discard)
		assert.ErrorContains(t, err, "webhook-cert-dir")
	})
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Options)
		wantErr string
	}{
		{"the defaults are valid", func(*Options) {}, ""},
		{
			"webhooks with no certificate directory",
			func(o *Options) { o.CertDir = "" },
			"webhook-cert-dir",
		},
		{
			"webhooks on an impossible port",
			func(o *Options) { o.WebhookPort = 70000 },
			"not a valid port",
		},
		{
			"webhooks on port zero",
			func(o *Options) { o.WebhookPort = 0 },
			"not a valid port",
		},
		{
			// Kubernetes has no way to notice a leader that has wedged if it
			// cannot probe it.
			"leader election with no health probe",
			func(o *Options) { o.ProbeAddr = "" },
			"health-probe-bind-address",
		},
		{
			"no webhooks means the certificate directory stops mattering",
			func(o *Options) {
				o.EnableWebhooks = false
				o.CertDir = ""
			},
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := DefaultOptions()
			tc.mutate(&opts)
			err := opts.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// The manager's client decodes everything it watches, so a scheme missing one
// of these types fails at runtime rather than at startup.
func TestNewSchemeCarriesEverythingTheControllerWatches(t *testing.T) {
	scheme := NewScheme()

	for _, gvk := range []schema.GroupVersionKind{
		tenancyv1alpha1.GroupVersion.WithKind("Tenant"),
		tenancyv1alpha1.GroupVersion.WithKind("TenantList"),
		appsv1.SchemeGroupVersion.WithKind("Deployment"),
		corev1.SchemeGroupVersion.WithKind("Namespace"),
		corev1.SchemeGroupVersion.WithKind("ResourceQuota"),
		corev1.SchemeGroupVersion.WithKind("Secret"),
		corev1.SchemeGroupVersion.WithKind("Service"),
		networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
	} {
		assert.True(t, scheme.Recognizes(gvk), "the scheme does not know about %s", gvk)
	}
}

func TestManagerOptions(t *testing.T) {
	opts := DefaultOptions()
	opts.MetricsAddr = ":7000"
	opts.ProbeAddr = ":7001"

	scheme := NewScheme()
	got := opts.ManagerOptions(scheme)

	assert.Same(t, scheme, got.Scheme)
	assert.Equal(t, ":7000", got.Metrics.BindAddress)
	assert.Equal(t, ":7001", got.HealthProbeBindAddress)
	assert.True(t, got.LeaderElection)
	assert.Equal(t, LeaderElectionID, got.LeaderElectionID)
	require.NotNil(t, got.WebhookServer)
}

// Setup is where a typo turns into an operator that starts cleanly and then
// silently watches nothing, so it is worth running against a real manager
// rather than asserting on the shape of the code.
func TestSetup(t *testing.T) {
	require.NoError(t, Setup(newTestManager(t), testOptions()))

	// Controller names are registered process-wide so that two controllers
	// cannot report the same metrics under the same name. A second
	// registration therefore has to fail loudly rather than produce an
	// operator that starts and quietly watches nothing.
	err := Setup(newTestManager(t), testOptions())
	require.Error(t, err)
	assert.ErrorContains(t, err, "tenant")
}

func TestRunFailsClearlyWithoutAKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "there-is-no-kubeconfig-here"))
	t.Setenv("HOME", t.TempDir())

	err := Run(context.Background(), testOptions())
	require.Error(t, err)
	assert.ErrorContains(t, err, "kubeconfig")
}

func testOptions() Options {
	opts := DefaultOptions()
	opts.LeaderElection = false
	opts.MetricsAddr = "0"
	opts.ProbeAddr = "0"
	opts.CertDir = "/tmp/tenant-operator-test-certs"
	return opts
}

// newTestManager builds a manager against an address nothing is listening on.
// controller-runtime resolves its REST mapper lazily, so this exercises all of
// the wiring without needing an API server for it.
func newTestManager(t *testing.T) ctrl.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(&rest.Config{Host: "127.0.0.1:1"}, ctrl.Options{
		Scheme:  NewScheme(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)
	return mgr
}
