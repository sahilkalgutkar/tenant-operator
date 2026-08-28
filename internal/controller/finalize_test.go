package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
)

// The teardown branches are the ones a real cluster makes awkward to reach on
// demand — a namespace that is already gone, a namespace somebody else owns —
// so this file drives them directly against a fake client. The happy path is
// covered against a real API server in the envtest suite instead; a fake client
// would happily accept objects the API server rejects.

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(tenancyv1alpha1.AddToScheme(s))
	return s
}

func newFakeReconciler(t *testing.T, objs ...client.Object) *TenantReconciler {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&tenancyv1alpha1.Tenant{}).
		Build()
	return &TenantReconciler{Client: c, Scheme: s}
}

func deletingTenant(mutate ...func(*tenancyv1alpha1.Tenant)) *tenancyv1alpha1.Tenant {
	now := metav1.Now()
	tenant := testTenant()
	tenant.DeletionTimestamp = &now
	tenant.Finalizers = []string{tenancyv1alpha1.Finalizer}
	for _, m := range mutate {
		m(tenant)
	}
	return tenant
}

func ownedNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: tenancyv1alpha1.OwnedLabels("acme", tenancyv1alpha1.TierStandard),
	}}
}

func TestFinalizeReleasesTheTenantOnceTheNamespaceIsGone(t *testing.T) {
	ctx := context.Background()
	tenant := deletingTenant()
	r := newFakeReconciler(t, tenant)

	res, err := r.finalize(ctx, tenant)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter, "there is nothing left to wait for")

	var got tenancyv1alpha1.Tenant
	err = r.Get(ctx, client.ObjectKey{Name: tenant.Name}, &got)
	assert.True(t, apierrors.IsNotFound(err), "the tenant should be gone, got %v", err)
}

func TestFinalizeDeletesTheNamespaceAndWaitsForIt(t *testing.T) {
	ctx := context.Background()
	tenant := deletingTenant()
	ns := ownedNamespace(tenant.NamespaceName())
	r := newFakeReconciler(t, tenant, ns)

	res, err := r.finalize(ctx, tenant)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "teardown has to be rechecked until the namespace is really gone")

	// And the tenant must still be there, saying what it is doing: that is the
	// entire point of holding the finalizer.
	var got tenancyv1alpha1.Tenant
	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: tenant.Name}, &got))
	assert.Contains(t, got.Finalizers, tenancyv1alpha1.Finalizer)
	assert.Equal(t, tenancyv1alpha1.PhaseTerminating, got.Status.Phase)
}

// A tenant pointed at somebody else's namespace must not take that namespace
// down with it when it is deleted.
func TestFinalizeLeavesAForeignNamespaceStanding(t *testing.T) {
	ctx := context.Background()
	tenant := deletingTenant(func(x *tenancyv1alpha1.Tenant) { x.Spec.Namespace = "someone-elses" })
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "someone-elses",
		Labels: map[string]string{"owner": "another-team"},
	}}
	r := newFakeReconciler(t, tenant, foreign)

	res, err := r.finalize(ctx, tenant)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)

	var stillThere corev1.Namespace
	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: "someone-elses"}, &stillThere))
	assert.True(t, stillThere.DeletionTimestamp.IsZero(), "the foreign namespace was deleted")
}

func TestFinalizeIsANoOpWithoutTheFinalizer(t *testing.T) {
	ctx := context.Background()
	tenant := deletingTenant(func(x *tenancyv1alpha1.Tenant) { x.Finalizers = nil })
	// The fake client will not hold an object with a deletion timestamp and no
	// finalizer, so this one is only ever passed in memory.
	r := newFakeReconciler(t)

	res, err := r.finalize(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}

func TestReconcileIgnoresATenantThatIsAlreadyGone(t *testing.T) {
	r := newFakeReconciler(t)
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "never-existed"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}

func TestReconcileAddsTheFinalizerBeforeCreatingAnything(t *testing.T) {
	ctx := context.Background()
	tenant := testTenant()
	r := newFakeReconciler(t, tenant)

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: tenant.Name}})
	require.NoError(t, err)
	assert.True(t, res.Requeue, "the first pass should requeue on the updated object")

	var got tenancyv1alpha1.Tenant
	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: tenant.Name}, &got))
	assert.Contains(t, got.Finalizers, tenancyv1alpha1.Finalizer)

	// Nothing else should exist yet: a tenant whose finalizer is not recorded
	// is a tenant whose namespace could be orphaned by a crash right here.
	var ns corev1.Namespace
	err = r.Get(ctx, client.ObjectKey{Name: tenant.NamespaceName()}, &ns)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestAssertNotForeign(t *testing.T) {
	tenant := testTenant()

	t.Run("a brand new object is fine", func(t *testing.T) {
		assert.NoError(t, assertNotForeign(tenant, &corev1.Service{}))
	})

	t.Run("an object this operator created is fine", func(t *testing.T) {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			UID:    "abc",
			Labels: tenancyv1alpha1.OwnedLabels(tenant.Name, tenancyv1alpha1.TierStandard),
		}}
		assert.NoError(t, assertNotForeign(tenant, svc))
	})

	t.Run("an object nobody labelled is refused", func(t *testing.T) {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{UID: "abc", Name: "api"}}
		assert.ErrorContains(t, assertNotForeign(tenant, svc), "refusing to adopt")
	})

	t.Run("another tenant's object is refused", func(t *testing.T) {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			UID:    "abc",
			Name:   "api",
			Labels: tenancyv1alpha1.OwnedLabels("globex", tenancyv1alpha1.TierStandard),
		}}
		assert.ErrorContains(t, assertNotForeign(tenant, svc), "belongs to tenant \"globex\"")
	})
}

func TestObserveWorkloadWhenTheDeploymentHasNotAppearedYet(t *testing.T) {
	tenant := testTenant()
	r := newFakeReconciler(t, tenant)

	require.NoError(t, r.observeWorkload(context.Background(), tenant))
	assert.Equal(t, tenancyv1alpha1.PhaseProvisioning, phaseFor(withNamespaceReady(tenant)))
}

func withNamespaceReady(t *tenancyv1alpha1.Tenant) *tenancyv1alpha1.Tenant {
	t.Status.Conditions = append(t.Status.Conditions, metav1.Condition{
		Type:               tenancyv1alpha1.ConditionNamespaceReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Provisioned",
		LastTransitionTime: metav1.Now(),
	})
	return t
}

// A reconciler with no recorder must not panic. It is a small thing, and it is
// the difference between a unit test that can construct the reconciler and one
// that cannot.
func TestEventsAreOptional(t *testing.T) {
	r := &TenantReconciler{}
	assert.NotPanics(t, func() {
		r.event(testTenant(), corev1.EventTypeNormal, "Test", "no recorder is attached")
	})
}
