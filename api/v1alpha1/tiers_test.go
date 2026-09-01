package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestPolicyForKnownTiers(t *testing.T) {
	for _, tier := range KnownTiers() {
		p := PolicyFor(tier)
		assert.Positive(t, p.MaxReplicas, "%s should allow at least one replica", tier)
		assert.LessOrEqual(t, p.DefaultReplicas, p.MaxReplicas,
			"%s defaults to more replicas than it allows", tier)
		assert.NotEmpty(t, p.CPULimit)
		assert.NotEmpty(t, p.MemoryLimit)
	}
}

// An unknown tier cannot arrive through the API — the CRD enum forbids it — so
// if one shows up it is a bug, and the safe response to a bug is the smallest
// slice of the cluster rather than the largest.
func TestPolicyForUnknownTierFallsBackToFree(t *testing.T) {
	assert.Equal(t, PolicyFor(TierFree), PolicyFor(TenantTier("platinum")))
	assert.Equal(t, PolicyFor(TierFree), PolicyFor(""))
}

func TestTiersAreOrderedSmallestFirst(t *testing.T) {
	tiers := KnownTiers()
	for i := 1; i < len(tiers); i++ {
		prev, cur := PolicyFor(tiers[i-1]), PolicyFor(tiers[i])
		assert.Less(t, prev.MaxReplicas, cur.MaxReplicas,
			"%s should allow more replicas than %s", tiers[i], tiers[i-1])
	}
}

func TestEffectiveReplicas(t *testing.T) {
	replicas := func(n int32) *int32 { return &n }

	cases := []struct {
		name string
		spec TenantSpec
		want int32
	}{
		{"default for the tier when unset", TenantSpec{Tier: TierStandard}, 2},
		{"default for enterprise when unset", TenantSpec{Tier: TierEnterprise}, 3},
		{"honours an explicit count", TenantSpec{Tier: TierStandard, Replicas: replicas(4)}, 4},
		{"clamps to the tier ceiling", TenantSpec{Tier: TierFree, Replicas: replicas(9)}, 1},
		{"suspension beats an explicit count", TenantSpec{Tier: TierEnterprise, Replicas: replicas(10), Suspended: true}, 0},
		{"suspension beats the tier default", TenantSpec{Tier: TierStandard, Suspended: true}, 0},
		{"zero is a legitimate request", TenantSpec{Tier: TierStandard, Replicas: replicas(0)}, 0},
		{"a negative count floors at zero", TenantSpec{Tier: TierStandard, Replicas: replicas(-3)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.spec.EffectiveReplicas())
		})
	}
}

func TestResourceRequirementsParseForEveryTier(t *testing.T) {
	for _, tier := range KnownTiers() {
		res := PolicyFor(tier).ResourceRequirements()
		cpuReq := res.Requests[corev1.ResourceCPU]
		cpuLim := res.Limits[corev1.ResourceCPU]
		memReq := res.Requests[corev1.ResourceMemory]
		memLim := res.Limits[corev1.ResourceMemory]

		require.False(t, cpuReq.IsZero(), "%s has a zero cpu request", tier)
		assert.LessOrEqual(t, cpuReq.MilliValue(), cpuLim.MilliValue(),
			"%s requests more cpu than it limits", tier)
		assert.LessOrEqual(t, memReq.Value(), memLim.Value(),
			"%s requests more memory than it limits", tier)
	}
}

// The quota has to leave room for one extra replica, otherwise a tenant sitting
// at its replica ceiling deadlocks against its own quota the moment it tries to
// roll out a new version.
func TestQuotaLeavesRoomForARollout(t *testing.T) {
	for _, tier := range KnownTiers() {
		p := PolicyFor(tier)
		hard := p.QuotaHard()

		perReplica := p.ResourceRequirements().Limits[corev1.ResourceCPU]
		total := hard[corev1.ResourceLimitsCPU]
		assert.Equal(t, perReplica.MilliValue()*int64(p.MaxReplicas+1), total.MilliValue(),
			"%s quota should cover max replicas plus one", tier)

		pods := hard[corev1.ResourcePods]
		assert.GreaterOrEqual(t, pods.Value(), int64(p.MaxReplicas),
			"%s pod quota is below its own replica ceiling", tier)
	}
}

func TestOnlyEnterpriseIsProtected(t *testing.T) {
	assert.True(t, PolicyFor(TierEnterprise).Protected)
	assert.False(t, PolicyFor(TierStandard).Protected)
	assert.False(t, PolicyFor(TierFree).Protected)
}

func TestNamespaceName(t *testing.T) {
	derived := &Tenant{}
	derived.Name = "acme"
	assert.Equal(t, "tenant-acme", derived.NamespaceName())

	pinned := &Tenant{Spec: TenantSpec{Namespace: "acme-prod"}}
	pinned.Name = "acme"
	assert.Equal(t, "acme-prod", pinned.NamespaceName())

	assert.Equal(t, "tenant-globex", DefaultNamespaceFor("globex"))
}

func TestEveryTierBoundsItsScratchVolume(t *testing.T) {
	var previous int64
	for _, tier := range KnownTiers() {
		p := PolicyFor(tier)
		require.NotEmpty(t, p.ScratchSize, "%s has no scratch size", tier)

		limit := p.ScratchSizeLimit()
		assert.Positive(t, limit.Value(), "%s scratch size must be a real cap", tier)
		assert.Greater(t, limit.Value(), previous,
			"%s should not get less scratch space than the tier below it", tier)
		previous = limit.Value()
	}
}
