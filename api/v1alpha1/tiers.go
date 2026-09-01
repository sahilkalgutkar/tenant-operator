package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TierPolicy is everything a tier decides. I keep it as data rather than as
// branches inside the controller so that adding a tier is a table entry and a
// test case, not a new code path through the reconcile loop.
type TierPolicy struct {
	// DefaultReplicas is what a tenant gets when it does not ask for a
	// specific number.
	DefaultReplicas int32
	// MaxReplicas is the ceiling. A tenant asking for more is rejected at
	// admission rather than silently clamped, because silently running fewer
	// replicas than the manifest says is the kind of thing nobody notices
	// until an incident.
	MaxReplicas int32
	// CPURequest and MemoryRequest are per-replica.
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string
	// QuotaPods and QuotaServices bound how much a tenant can create inside
	// its own namespace, independently of what I create for it.
	QuotaPods     int64
	QuotaServices int64
	// ScratchSize caps the writable /tmp every workload pod gets. The root
	// filesystem is read-only, so a container that needs to write anything at
	// all writes it here -- and an emptyDir with no size limit is node
	// ephemeral storage a tenant could fill without ever touching its own
	// quota, which is the one resource the namespace quota does not bound.
	ScratchSize string
	// Protected means deletion requires an explicit confirmation annotation.
	Protected bool
}

// tierPolicies is the single source of truth for tier behaviour.
var tierPolicies = map[TenantTier]TierPolicy{
	TierFree: {
		DefaultReplicas: 1,
		MaxReplicas:     1,
		CPURequest:      "50m",
		MemoryRequest:   "64Mi",
		CPULimit:        "200m",
		MemoryLimit:     "128Mi",
		QuotaPods:       4,
		QuotaServices:   2,
		ScratchSize:     "64Mi",
	},
	TierStandard: {
		DefaultReplicas: 2,
		MaxReplicas:     5,
		CPURequest:      "100m",
		MemoryRequest:   "128Mi",
		CPULimit:        "500m",
		MemoryLimit:     "512Mi",
		QuotaPods:       20,
		QuotaServices:   10,
		ScratchSize:     "256Mi",
	},
	TierEnterprise: {
		DefaultReplicas: 3,
		MaxReplicas:     20,
		CPURequest:      "250m",
		MemoryRequest:   "512Mi",
		CPULimit:        "2",
		MemoryLimit:     "2Gi",
		QuotaPods:       100,
		QuotaServices:   50,
		ScratchSize:     "1Gi",
		Protected:       true,
	},
}

// PolicyFor returns the policy for a tier. An unknown tier falls back to the
// most restrictive one rather than erroring: the enum validation on the CRD
// already makes an unknown tier unreachable through the API, so if one ever
// does show up it came from a bug, and the safe response to a bug is to hand
// out the smallest slice of the cluster rather than the largest.
func PolicyFor(t TenantTier) TierPolicy {
	if p, ok := tierPolicies[t]; ok {
		return p
	}
	return tierPolicies[TierFree]
}

// KnownTiers lists the tiers I serve, smallest first.
func KnownTiers() []TenantTier {
	return []TenantTier{TierFree, TierStandard, TierEnterprise}
}

// ResourceRequirements is the per-replica container resource block for a tier.
func (p TierPolicy) ResourceRequirements() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(p.CPURequest),
			corev1.ResourceMemory: resource.MustParse(p.MemoryRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(p.CPULimit),
			corev1.ResourceMemory: resource.MustParse(p.MemoryLimit),
		},
	}
}

// ScratchSizeLimit is the size cap on the workload's writable /tmp volume.
func (p TierPolicy) ScratchSizeLimit() resource.Quantity {
	return resource.MustParse(p.ScratchSize)
}

// QuotaHard is the namespace-wide ResourceQuota for a tier. I scale the
// aggregate CPU and memory off the per-replica limits and the replica ceiling,
// with headroom, so a tenant that scales to its maximum does not immediately
// wedge itself against its own quota.
func (p TierPolicy) QuotaHard() corev1.ResourceList {
	cpuLimit := resource.MustParse(p.CPULimit)
	memLimit := resource.MustParse(p.MemoryLimit)
	cpuRequest := resource.MustParse(p.CPURequest)
	memRequest := resource.MustParse(p.MemoryRequest)

	// One replica of headroom on top of the ceiling, so a tenant sitting at
	// its maximum can still roll out a new version rather than deadlocking
	// against its own quota mid-deploy.
	replicas := int64(p.MaxReplicas) + 1
	totalCPU := resource.NewMilliQuantity(cpuLimit.MilliValue()*replicas, resource.DecimalSI)
	totalMem := resource.NewQuantity(memLimit.Value()*replicas, resource.BinarySI)

	return corev1.ResourceList{
		corev1.ResourceLimitsCPU:      *totalCPU,
		corev1.ResourceLimitsMemory:   *totalMem,
		corev1.ResourcePods:           *resource.NewQuantity(p.QuotaPods, resource.DecimalSI),
		corev1.ResourceServices:       *resource.NewQuantity(p.QuotaServices, resource.DecimalSI),
		corev1.ResourceRequestsCPU:    *resource.NewMilliQuantity(cpuRequest.MilliValue()*replicas, resource.DecimalSI),
		corev1.ResourceRequestsMemory: *resource.NewQuantity(memRequest.Value()*replicas, resource.BinarySI),
	}
}

// EffectiveReplicas resolves the spec down to the number of replicas the
// Deployment should actually carry. Suspension wins over everything, and the
// tier ceiling wins over the requested count.
func (s TenantSpec) EffectiveReplicas() int32 {
	if s.Suspended {
		return 0
	}
	p := PolicyFor(s.Tier)
	if s.Replicas == nil {
		return p.DefaultReplicas
	}
	if *s.Replicas > p.MaxReplicas {
		return p.MaxReplicas
	}
	if *s.Replicas < 0 {
		return 0
	}
	return *s.Replicas
}

// NamespaceName is the namespace this tenant belongs in, falling back to the
// derived default when the spec does not pin one.
func (t *Tenant) NamespaceName() string {
	if t.Spec.Namespace != "" {
		return t.Spec.Namespace
	}
	return DefaultNamespaceFor(t.Name)
}

// DefaultNamespaceFor derives a tenant's namespace name from its object name.
func DefaultNamespaceFor(name string) string {
	return fmt.Sprintf("tenant-%s", name)
}
