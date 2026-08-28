package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
)

// These tests run the real reconciler against a real API server that envtest
// starts for them. A fake client would let me assert that I called the methods
// I meant to call; only an API server tells me whether the objects I build are
// actually accepted, whether an owner reference from a cluster-scoped Tenant to
// a namespaced Deployment is legal, and whether a Deployment I update a second
// time still validates.

var (
	k8sClient client.Client
	restCfg   *rest.Config
	testEnv   *envtest.Environment
	envReady  bool
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr,
			"KUBEBUILDER_ASSETS is not set, so the envtest suite cannot start an API server.\n"+
				"Run `make test`, which downloads the control-plane binaries first.")
		os.Exit(m.Run())
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		os.Exit(1)
	}
	restCfg = cfg

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(tenancyv1alpha1.AddToScheme(scheme))

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "building the test client: %v\n", err)
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "building the manager: %v\n", err)
		os.Exit(1)
	}

	reconciler := &TenantReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("tenant-controller"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setting up the controller: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager stopped: %v\n", err)
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		fmt.Fprintln(os.Stderr, "the manager cache never synced")
		os.Exit(1)
	}
	envReady = true

	code := m.Run()

	cancel()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}

// requireEnv skips a test that needs the API server when the control-plane
// binaries are not installed, rather than failing with an opaque connection
// error a reader would have to go and decode.
func requireEnv(t *testing.T) {
	t.Helper()
	if !envReady {
		t.Skip("envtest is not available: run `make test`")
	}
}

// eventually polls until the condition holds or the deadline passes. The
// controller is asynchronous, so every assertion about what it did has to be
// written this way; asserting once, immediately, would only ever test how fast
// the workqueue happened to be on that run.
func eventually(t *testing.T, timeout time.Duration, describe string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = check(); last == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", describe, last)
}

// consistently asserts something stays true, which is how I check that the
// controller does *not* do something — a single check would pass simply
// because the reconcile had not happened yet.
func consistently(t *testing.T, d time.Duration, describe string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := check(); err != nil {
			t.Fatalf("%s did not hold: %v", describe, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func objectKey(name, namespace string) client.ObjectKey {
	return client.ObjectKey{Name: name, Namespace: namespace}
}
