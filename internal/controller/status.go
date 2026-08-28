package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
)

// phaseFor derives the human-facing phase from the conditions. The conditions
// are the source of truth and the phase is a projection of them — I never set
// the phase independently, because two fields that can disagree about the same
// fact will eventually disagree.
func phaseFor(t *tenancyv1alpha1.Tenant) tenancyv1alpha1.TenantPhase {
	if !t.DeletionTimestamp.IsZero() {
		return tenancyv1alpha1.PhaseTerminating
	}

	ns := conditionStatus(t, tenancyv1alpha1.ConditionNamespaceReady)
	wl := conditionStatus(t, tenancyv1alpha1.ConditionWorkloadReady)

	switch {
	case ns == metav1.ConditionFalse:
		return tenancyv1alpha1.PhaseDegraded
	case ns == metav1.ConditionUnknown || wl == metav1.ConditionUnknown:
		return tenancyv1alpha1.PhasePending
	case wl == metav1.ConditionTrue && t.Spec.Suspended:
		return tenancyv1alpha1.PhaseSuspended
	case wl == metav1.ConditionTrue:
		return tenancyv1alpha1.PhaseReady
	default:
		// The namespace is up but the workload is not there yet. That is the
		// normal state during a rollout, and calling it Degraded would page
		// somebody every time a tenant scaled up.
		return tenancyv1alpha1.PhaseProvisioning
	}
}

func conditionStatus(t *tenancyv1alpha1.Tenant, condType string) metav1.ConditionStatus {
	c := meta.FindStatusCondition(t.Status.Conditions, condType)
	if c == nil {
		return metav1.ConditionUnknown
	}
	return c.Status
}

// readyCondition rolls the component conditions up into the one condition an
// external tool should wait on. `kubectl wait --for=condition=Ready tenant/acme`
// has to mean something, and it can only mean something if Ready is False
// whenever any part of the tenant is not there.
func readyCondition(t *tenancyv1alpha1.Tenant) metav1.Condition {
	ns := meta.FindStatusCondition(t.Status.Conditions, tenancyv1alpha1.ConditionNamespaceReady)
	wl := meta.FindStatusCondition(t.Status.Conditions, tenancyv1alpha1.ConditionWorkloadReady)

	for _, c := range []*metav1.Condition{ns, wl} {
		if c == nil {
			return metav1.Condition{
				Type:    tenancyv1alpha1.ConditionReady,
				Status:  metav1.ConditionUnknown,
				Reason:  "NotObserved",
				Message: "waiting for the first full reconcile",
			}
		}
		if c.Status != metav1.ConditionTrue {
			return metav1.Condition{
				Type:    tenancyv1alpha1.ConditionReady,
				Status:  c.Status,
				Reason:  c.Reason,
				Message: c.Message,
			}
		}
	}

	if t.Spec.Suspended {
		return metav1.Condition{
			Type:    tenancyv1alpha1.ConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  "Suspended",
			Message: "tenant is suspended: workload scaled to zero, namespace and data retained",
		}
	}
	return metav1.Condition{
		Type:    tenancyv1alpha1.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Provisioned",
		Message: "namespace, guard rails and workload are all in place",
	}
}
