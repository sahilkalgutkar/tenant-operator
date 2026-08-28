package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
)

func withConditions(t *tenancyv1alpha1.Tenant, conds ...metav1.Condition) *tenancyv1alpha1.Tenant {
	t.Status.Conditions = conds
	return t
}

func cond(condType string, status metav1.ConditionStatus) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: "Test", LastTransitionTime: metav1.Now()}
}

func TestPhaseFor(t *testing.T) {
	deleting := testTenant()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{tenancyv1alpha1.Finalizer}

	suspended := testTenant(func(x *tenancyv1alpha1.Tenant) { x.Spec.Suspended = true })

	cases := []struct {
		name    string
		subject *tenancyv1alpha1.Tenant
		want    tenancyv1alpha1.TenantPhase
	}{
		{
			"deletion wins over everything",
			withConditions(deleting,
				cond(tenancyv1alpha1.ConditionNamespaceReady, metav1.ConditionTrue),
				cond(tenancyv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue)),
			tenancyv1alpha1.PhaseTerminating,
		},
		{
			"nothing observed yet",
			testTenant(),
			tenancyv1alpha1.PhasePending,
		},
		{
			"a broken namespace is degraded",
			withConditions(testTenant(),
				cond(tenancyv1alpha1.ConditionNamespaceReady, metav1.ConditionFalse),
				cond(tenancyv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse)),
			tenancyv1alpha1.PhaseDegraded,
		},
		{
			"a namespace up but no workload yet is provisioning, not degraded",
			withConditions(testTenant(),
				cond(tenancyv1alpha1.ConditionNamespaceReady, metav1.ConditionTrue),
				cond(tenancyv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse)),
			tenancyv1alpha1.PhaseProvisioning,
		},
		{
			"everything up",
			withConditions(testTenant(),
				cond(tenancyv1alpha1.ConditionNamespaceReady, metav1.ConditionTrue),
				cond(tenancyv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue)),
			tenancyv1alpha1.PhaseReady,
		},
		{
			"a settled suspended tenant reads as suspended, not ready",
			withConditions(suspended,
				cond(tenancyv1alpha1.ConditionNamespaceReady, metav1.ConditionTrue),
				cond(tenancyv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue)),
			tenancyv1alpha1.PhaseSuspended,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, phaseFor(tc.subject))
		})
	}
}

// `kubectl wait --for=condition=Ready tenant/acme` has to mean something, so
// Ready must be False whenever any component of the tenant is missing.
func TestReadyConditionRollsUpTheComponents(t *testing.T) {
	t.Run("unknown until the first full reconcile", func(t *testing.T) {
		got := readyCondition(testTenant())
		assert.Equal(t, metav1.ConditionUnknown, got.Status)
		assert.Equal(t, "NotObserved", got.Reason)
	})

	t.Run("inherits the reason of whichever component is failing", func(t *testing.T) {
		subject := withConditions(testTenant(),
			cond(tenancyv1alpha1.ConditionNamespaceReady, metav1.ConditionTrue),
			metav1.Condition{
				Type:    tenancyv1alpha1.ConditionWorkloadReady,
				Status:  metav1.ConditionFalse,
				Reason:  "ReplicasNotReady",
				Message: "1/3 replicas ready",
			})
		got := readyCondition(subject)
		assert.Equal(t, metav1.ConditionFalse, got.Status)
		assert.Equal(t, "ReplicasNotReady", got.Reason)
		assert.Equal(t, "1/3 replicas ready", got.Message)
	})

	t.Run("a namespace failure short-circuits before the workload", func(t *testing.T) {
		subject := withConditions(testTenant(),
			metav1.Condition{Type: tenancyv1alpha1.ConditionNamespaceReady, Status: metav1.ConditionFalse, Reason: "GuardRailError"},
			cond(tenancyv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue))
		assert.Equal(t, "GuardRailError", readyCondition(subject).Reason)
	})

	t.Run("ready when everything is up", func(t *testing.T) {
		subject := withConditions(testTenant(),
			cond(tenancyv1alpha1.ConditionNamespaceReady, metav1.ConditionTrue),
			cond(tenancyv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue))
		got := readyCondition(subject)
		assert.Equal(t, metav1.ConditionTrue, got.Status)
		assert.Equal(t, "Provisioned", got.Reason)
	})

	// A suspended tenant is doing exactly what it was asked to do, so it is
	// Ready — reporting it as not-Ready would make every dashboard show a
	// permanent fault for a deliberate state.
	t.Run("a suspended tenant is ready, with a reason that says so", func(t *testing.T) {
		subject := withConditions(
			testTenant(func(x *tenancyv1alpha1.Tenant) { x.Spec.Suspended = true }),
			cond(tenancyv1alpha1.ConditionNamespaceReady, metav1.ConditionTrue),
			cond(tenancyv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue))
		got := readyCondition(subject)
		assert.Equal(t, metav1.ConditionTrue, got.Status)
		assert.Equal(t, "Suspended", got.Reason)
	})
}
