package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
)

const (
	settleTimeout = 20 * time.Second
	holdFor       = 700 * time.Millisecond
)

// createTenant applies the defaults the webhook would have applied (the
// webhook server is not running in this suite) and creates the object.
func createTenant(t *testing.T, ctx context.Context, name string, mutate ...func(*tenancyv1alpha1.Tenant)) *tenancyv1alpha1.Tenant {
	t.Helper()
	tenant := &tenancyv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: tenancyv1alpha1.TenantSpec{
			DisplayName: name,
			Tier:        tenancyv1alpha1.TierStandard,
			Image:       "ghcr.io/example/app:1.4.2",
		},
	}
	for _, m := range mutate {
		m(tenant)
	}
	tenancyv1alpha1.ApplyDefaults(tenant)
	require.NoError(t, k8sClient.Create(ctx, tenant))
	t.Cleanup(func() { cleanupTenant(t, tenant.Name) })
	return tenant
}

// cleanupTenant tears a tenant down at the end of a test. envtest runs no
// namespace controller, so a namespace that is deleted stays Terminating
// forever unless its kubernetes finalizer is cleared by hand — which is what
// the real controller is waiting on in production, and what I stand in for
// here.
func cleanupTenant(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()

	var tenant tenancyv1alpha1.Tenant
	if err := k8sClient.Get(ctx, objectKey(name, ""), &tenant); err != nil {
		return
	}
	if tenant.DeletionTimestamp.IsZero() {
		_ = k8sClient.Delete(ctx, &tenant)
	}
	// Best effort: a tenant that refused to adopt its namespace never asked
	// for it to be deleted, so there may be nothing to finalize.
	tryFinalizeNamespace(tenant.NamespaceName(), 3*time.Second)

	eventually(t, settleTimeout, "the tenant to be removed", func() error {
		err := k8sClient.Get(ctx, objectKey(name, ""), &tenant)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("tenant %s is still present", name)
	})
}

// finalizeNamespace does what the namespace controller would do in a real
// cluster: once everything inside a terminating namespace is gone, it clears
// the kubernetes finalizer so the namespace object can be removed.
func finalizeNamespace(t *testing.T, name string) {
	t.Helper()
	eventually(t, settleTimeout, "the namespace to finish terminating", func() error {
		return finalizeNamespaceOnce(name)
	})
}

func tryFinalizeNamespace(name string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if finalizeNamespaceOnce(name) == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func finalizeNamespaceOnce(name string) error {
	ctx := context.Background()

	var ns corev1.Namespace
	err := k8sClient.Get(ctx, objectKey(name, ""), &ns)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.DeletionTimestamp.IsZero() {
		return fmt.Errorf("namespace %s has not been deleted yet", name)
	}
	if len(ns.Spec.Finalizers) == 0 {
		return nil
	}
	ns.Spec.Finalizers = nil
	return k8sClient.SubResource("finalize").Update(ctx, &ns)
}

func TestTenantProvisionsANamespaceWithGuardRails(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	tenant := createTenant(t, ctx, "provision-acme")
	ns := tenant.NamespaceName()

	eventually(t, settleTimeout, "the namespace", func() error {
		var got corev1.Namespace
		if err := k8sClient.Get(ctx, objectKey(ns, ""), &got); err != nil {
			return err
		}
		if got.Labels[tenancyv1alpha1.LabelTenant] != tenant.Name {
			return fmt.Errorf("namespace is not labelled for the tenant")
		}
		if got.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
			return fmt.Errorf("pod security is not enforced on the namespace")
		}
		return nil
	})

	// The guard rails matter more than the workload: a tenant namespace with
	// no quota and no network policy is a naming convention, not a boundary.
	eventually(t, settleTimeout, "the resource quota", func() error {
		var quota corev1.ResourceQuota
		if err := k8sClient.Get(ctx, objectKey(quotaName, ns), &quota); err != nil {
			return err
		}
		want := tenancyv1alpha1.PolicyFor(tenancyv1alpha1.TierStandard).QuotaHard()
		got := quota.Spec.Hard[corev1.ResourcePods]
		expected := want[corev1.ResourcePods]
		if got.Cmp(expected) != 0 {
			return fmt.Errorf("quota does not match the tier: %v", quota.Spec.Hard)
		}
		return nil
	})

	eventually(t, settleTimeout, "the network policy", func() error {
		var np networkingv1.NetworkPolicy
		return k8sClient.Get(ctx, objectKey(networkPolicyName, ns), &np)
	})

	eventually(t, settleTimeout, "the workload", func() error {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, objectKey(WorkloadName(tenant.Name), ns), &deploy); err != nil {
			return err
		}
		if *deploy.Spec.Replicas != 2 {
			return fmt.Errorf("expected 2 replicas, got %d", *deploy.Spec.Replicas)
		}
		var svc corev1.Service
		return k8sClient.Get(ctx, objectKey(WorkloadName(tenant.Name), ns), &svc)
	})

	// Everything must be owned by the Tenant, so that garbage collection is a
	// backstop underneath the finalizer rather than the only mechanism.
	var deploy appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, objectKey(WorkloadName(tenant.Name), ns), &deploy))
	require.Len(t, deploy.OwnerReferences, 1)
	assert.Equal(t, "Tenant", deploy.OwnerReferences[0].Kind)
	assert.Equal(t, tenant.Name, deploy.OwnerReferences[0].Name)
	assert.True(t, *deploy.OwnerReferences[0].Controller)
}

func TestTenantStatusReflectsTheWorkload(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	tenant := createTenant(t, ctx, "status-acme")

	// envtest runs no kubelet and no deployment controller, so no pod will
	// ever become ready on its own. Until I say otherwise, the tenant must
	// report itself as still provisioning rather than as Ready.
	eventually(t, settleTimeout, "the provisioning phase", func() error {
		var got tenancyv1alpha1.Tenant
		if err := k8sClient.Get(ctx, objectKey(tenant.Name, ""), &got); err != nil {
			return err
		}
		if got.Status.Phase != tenancyv1alpha1.PhaseProvisioning {
			return fmt.Errorf("phase is %q", got.Status.Phase)
		}
		if got.Status.DesiredReplicas != 2 || got.Status.ReadyReplicas != 0 {
			return fmt.Errorf("replica counts are %d/%d", got.Status.ReadyReplicas, got.Status.DesiredReplicas)
		}
		if !meta.IsStatusConditionFalse(got.Status.Conditions, tenancyv1alpha1.ConditionReady) {
			return fmt.Errorf("Ready should be false while replicas are missing")
		}
		return nil
	})

	// Now stand in for the deployment controller.
	markReplicasReady(t, ctx, tenant, 2)

	eventually(t, settleTimeout, "the ready phase", func() error {
		var got tenancyv1alpha1.Tenant
		if err := k8sClient.Get(ctx, objectKey(tenant.Name, ""), &got); err != nil {
			return err
		}
		if got.Status.Phase != tenancyv1alpha1.PhaseReady {
			return fmt.Errorf("phase is %q", got.Status.Phase)
		}
		if !meta.IsStatusConditionTrue(got.Status.Conditions, tenancyv1alpha1.ConditionReady) {
			return fmt.Errorf("Ready is not true")
		}
		if got.Status.ObservedGeneration != got.Generation {
			return fmt.Errorf("status is stale: observed %d, generation %d",
				got.Status.ObservedGeneration, got.Generation)
		}
		if got.Status.CredentialsSecret != CredentialsSecretName(tenant.Name) {
			return fmt.Errorf("status does not point at the credentials secret")
		}
		return nil
	})
}

func markReplicasReady(t *testing.T, ctx context.Context, tenant *tenancyv1alpha1.Tenant, n int32) {
	t.Helper()
	eventually(t, settleTimeout, "the deployment status to be writable", func() error {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, objectKey(WorkloadName(tenant.Name), tenant.NamespaceName()), &deploy); err != nil {
			return err
		}
		deploy.Status.Replicas = n
		deploy.Status.ReadyReplicas = n
		deploy.Status.AvailableReplicas = n
		return k8sClient.Status().Update(ctx, &deploy)
	})
}

// This is the property that makes it a controller rather than a provisioning
// script: whatever anybody does to the objects it owns, the next reconcile puts
// them back.
func TestControllerCorrectsDrift(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	tenant := createTenant(t, ctx, "drift-acme")
	ns := tenant.NamespaceName()
	key := objectKey(WorkloadName(tenant.Name), ns)

	eventually(t, settleTimeout, "the initial deployment", func() error {
		var deploy appsv1.Deployment
		return k8sClient.Get(ctx, key, &deploy)
	})

	t.Run("a hand-scaled deployment is put back", func(t *testing.T) {
		var deploy appsv1.Deployment
		require.NoError(t, k8sClient.Get(ctx, key, &deploy))
		scaled := int32(7)
		deploy.Spec.Replicas = &scaled
		require.NoError(t, k8sClient.Update(ctx, &deploy))

		eventually(t, settleTimeout, "the replica count to be restored", func() error {
			var got appsv1.Deployment
			if err := k8sClient.Get(ctx, key, &got); err != nil {
				return err
			}
			if *got.Spec.Replicas != 2 {
				return fmt.Errorf("replicas are %d", *got.Spec.Replicas)
			}
			return nil
		})
	})

	t.Run("an edited image is put back", func(t *testing.T) {
		var deploy appsv1.Deployment
		require.NoError(t, k8sClient.Get(ctx, key, &deploy))
		deploy.Spec.Template.Spec.Containers[0].Image = "ghcr.io/example/somebody-elses-app:9.9.9"
		require.NoError(t, k8sClient.Update(ctx, &deploy))

		eventually(t, settleTimeout, "the image to be restored", func() error {
			var got appsv1.Deployment
			if err := k8sClient.Get(ctx, key, &got); err != nil {
				return err
			}
			if got.Spec.Template.Spec.Containers[0].Image != "ghcr.io/example/app:1.4.2" {
				return fmt.Errorf("image is %s", got.Spec.Template.Spec.Containers[0].Image)
			}
			return nil
		})
	})

	t.Run("a deleted service comes back", func(t *testing.T) {
		var svc corev1.Service
		require.NoError(t, k8sClient.Get(ctx, key, &svc))
		require.NoError(t, k8sClient.Delete(ctx, &svc))

		eventually(t, settleTimeout, "the service to be recreated", func() error {
			var got corev1.Service
			return k8sClient.Get(ctx, key, &got)
		})
	})
}

// Suspension is the reason the tier and the replica count are resolved in one
// place: it has to win over both of them.
func TestSuspendingATenantScalesItToZeroWithoutLosingAnything(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	tenant := createTenant(t, ctx, "suspend-acme")
	ns := tenant.NamespaceName()

	var secretBefore corev1.Secret
	eventually(t, settleTimeout, "the credentials secret", func() error {
		return k8sClient.Get(ctx, objectKey(CredentialsSecretName(tenant.Name), ns), &secretBefore)
	})

	require.NoError(t, k8sClient.Get(ctx, objectKey(tenant.Name, ""), tenant))
	tenant.Spec.Suspended = true
	require.NoError(t, k8sClient.Update(ctx, tenant))

	eventually(t, settleTimeout, "the workload to scale to zero", func() error {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, objectKey(WorkloadName(tenant.Name), ns), &deploy); err != nil {
			return err
		}
		if *deploy.Spec.Replicas != 0 {
			return fmt.Errorf("replicas are %d", *deploy.Spec.Replicas)
		}
		return nil
	})

	eventually(t, settleTimeout, "the suspended phase", func() error {
		var got tenancyv1alpha1.Tenant
		if err := k8sClient.Get(ctx, objectKey(tenant.Name, ""), &got); err != nil {
			return err
		}
		if got.Status.Phase != tenancyv1alpha1.PhaseSuspended {
			return fmt.Errorf("phase is %q", got.Status.Phase)
		}
		// A suspended tenant is doing what it was told to do, so it is Ready.
		if !meta.IsStatusConditionTrue(got.Status.Conditions, tenancyv1alpha1.ConditionReady) {
			return fmt.Errorf("a suspended tenant should still be Ready")
		}
		return nil
	})

	// Suspension must not cost the tenant anything it cannot get back.
	var secretAfter corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, objectKey(CredentialsSecretName(tenant.Name), ns), &secretAfter))
	assert.Equal(t, secretBefore.Data[apiKeyField], secretAfter.Data[apiKeyField])

	var quota corev1.ResourceQuota
	assert.NoError(t, k8sClient.Get(ctx, objectKey(quotaName, ns), &quota))
}

// Rotating the API key on every reconcile would technically converge, and would
// also break the tenant's workload every time anything about it changed.
func TestReconcilingNeverRotatesTheAPIKey(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	tenant := createTenant(t, ctx, "keystable-acme")
	secretKey := objectKey(CredentialsSecretName(tenant.Name), tenant.NamespaceName())

	var original corev1.Secret
	eventually(t, settleTimeout, "the credentials secret", func() error {
		return k8sClient.Get(ctx, secretKey, &original)
	})
	require.NotEmpty(t, original.Data[apiKeyField])

	for i := 0; i < 3; i++ {
		require.NoError(t, k8sClient.Get(ctx, objectKey(tenant.Name, ""), tenant))
		tenant.Spec.DisplayName = fmt.Sprintf("Acme Corp %d", i)
		require.NoError(t, k8sClient.Update(ctx, tenant))
	}

	consistently(t, holdFor, "the api key", func() error {
		var got corev1.Secret
		if err := k8sClient.Get(ctx, secretKey, &got); err != nil {
			return err
		}
		if string(got.Data[apiKeyField]) != string(original.Data[apiKeyField]) {
			return fmt.Errorf("the api key was rotated")
		}
		return nil
	})
}

// A tier upgrade has to move the quota and the pod resources without touching
// the Deployment's selector, which the API server will not let me change.
func TestUpgradingATierWidensTheQuotaWithoutBreakingTheDeployment(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	tenant := createTenant(t, ctx, "upgrade-acme", func(x *tenancyv1alpha1.Tenant) {
		x.Spec.Tier = tenancyv1alpha1.TierFree
	})
	ns := tenant.NamespaceName()

	var selectorBefore *metav1.LabelSelector
	eventually(t, settleTimeout, "the free-tier deployment", func() error {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, objectKey(WorkloadName(tenant.Name), ns), &deploy); err != nil {
			return err
		}
		if *deploy.Spec.Replicas != 1 {
			return fmt.Errorf("free tier should run 1 replica, got %d", *deploy.Spec.Replicas)
		}
		selectorBefore = deploy.Spec.Selector.DeepCopy()
		return nil
	})

	require.NoError(t, k8sClient.Get(ctx, objectKey(tenant.Name, ""), tenant))
	tenant.Spec.Tier = tenancyv1alpha1.TierEnterprise
	tenant.Spec.Replicas = nil
	require.NoError(t, k8sClient.Update(ctx, tenant))

	eventually(t, settleTimeout, "the enterprise quota and replica count", func() error {
		var quota corev1.ResourceQuota
		if err := k8sClient.Get(ctx, objectKey(quotaName, ns), &quota); err != nil {
			return err
		}
		want := tenancyv1alpha1.PolicyFor(tenancyv1alpha1.TierEnterprise).QuotaHard()
		gotPods := quota.Spec.Hard[corev1.ResourcePods]
		wantPods := want[corev1.ResourcePods]
		if gotPods.Cmp(wantPods) != 0 {
			return fmt.Errorf("quota is still %v", gotPods.String())
		}

		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, objectKey(WorkloadName(tenant.Name), ns), &deploy); err != nil {
			return err
		}
		if *deploy.Spec.Replicas != 3 {
			return fmt.Errorf("enterprise tier should run 3 replicas, got %d", *deploy.Spec.Replicas)
		}
		if deploy.Spec.Template.Spec.Containers[0].Resources.Limits.Cpu().String() != "2" {
			return fmt.Errorf("container resources did not move to the new tier")
		}
		if !equalSelectors(selectorBefore, deploy.Spec.Selector) {
			return fmt.Errorf("the immutable selector changed")
		}
		return nil
	})
}

func equalSelectors(a, b *metav1.LabelSelector) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.MatchLabels) != len(b.MatchLabels) {
		return false
	}
	for k, v := range a.MatchLabels {
		if b.MatchLabels[k] != v {
			return false
		}
	}
	return true
}

// The whole point of the finalizer: the Tenant object outlives the delete call
// until its namespace has genuinely gone, so anything scripted around
// `kubectl delete tenant` is not racing the teardown.
func TestDeletingATenantBlocksUntilItsNamespaceIsGone(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	tenant := createTenant(t, ctx, "teardown-acme")
	ns := tenant.NamespaceName()

	eventually(t, settleTimeout, "the namespace", func() error {
		var got corev1.Namespace
		return k8sClient.Get(ctx, objectKey(ns, ""), &got)
	})

	require.NoError(t, k8sClient.Get(ctx, objectKey(tenant.Name, ""), tenant))
	require.Contains(t, tenant.Finalizers, tenancyv1alpha1.Finalizer)
	require.NoError(t, k8sClient.Delete(ctx, tenant))

	// The namespace goes into termination and the Tenant stays put, reporting
	// Terminating, for as long as that takes.
	eventually(t, settleTimeout, "the namespace to be deleting and the tenant to survive", func() error {
		var got tenancyv1alpha1.Tenant
		if err := k8sClient.Get(ctx, objectKey(tenant.Name, ""), &got); err != nil {
			return fmt.Errorf("the tenant disappeared before its namespace did: %w", err)
		}
		if got.Status.Phase != tenancyv1alpha1.PhaseTerminating {
			return fmt.Errorf("tenant reports phase %q while it is being torn down", got.Status.Phase)
		}
		var namespace corev1.Namespace
		if err := k8sClient.Get(ctx, objectKey(ns, ""), &namespace); err != nil {
			return err
		}
		if namespace.DeletionTimestamp.IsZero() {
			return fmt.Errorf("the namespace has not been deleted")
		}
		return nil
	})

	// Stand in for the namespace controller, then the finalizer should clear
	// and the Tenant should go.
	finalizeNamespace(t, ns)

	eventually(t, settleTimeout, "the tenant to finish deleting", func() error {
		var got tenancyv1alpha1.Tenant
		err := k8sClient.Get(ctx, objectKey(tenant.Name, ""), &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("the tenant is still present with finalizers %v", got.Finalizers)
	})
}

// A tenant pointed at a namespace somebody else already created must not
// silently take it over — and, more importantly, must not delete it later.
func TestControllerRefusesToAdoptAForeignNamespace(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()

	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "someone-elses-namespace",
		Labels: map[string]string{"owner": "another-team"},
	}}
	require.NoError(t, k8sClient.Create(ctx, existing))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), existing)
		finalizeNamespace(t, existing.Name)
	})

	tenant := createTenant(t, ctx, "adopt-acme", func(x *tenancyv1alpha1.Tenant) {
		x.Spec.Namespace = existing.Name
	})

	eventually(t, settleTimeout, "the tenant to report the refusal", func() error {
		var got tenancyv1alpha1.Tenant
		if err := k8sClient.Get(ctx, objectKey(tenant.Name, ""), &got); err != nil {
			return err
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, tenancyv1alpha1.ConditionNamespaceReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			return fmt.Errorf("NamespaceReady is not false: %+v", cond)
		}
		if got.Status.Phase != tenancyv1alpha1.PhaseDegraded {
			return fmt.Errorf("phase is %q", got.Status.Phase)
		}
		return nil
	})

	// And it must leave the namespace exactly as it found it.
	var after corev1.Namespace
	require.NoError(t, k8sClient.Get(ctx, objectKey(existing.Name, ""), &after))
	assert.NotContains(t, after.Labels, tenancyv1alpha1.LabelTenant)
	assert.Empty(t, after.OwnerReferences)
}
