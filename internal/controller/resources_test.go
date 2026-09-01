package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
)

func testTenant(mutate ...func(*tenancyv1alpha1.Tenant)) *tenancyv1alpha1.Tenant {
	t := &tenancyv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "acme"},
		Spec: tenancyv1alpha1.TenantSpec{
			DisplayName: "Acme Corp",
			Tier:        tenancyv1alpha1.TierStandard,
			Image:       "ghcr.io/example/app:1.4.2",
		},
	}
	for _, m := range mutate {
		m(t)
	}
	return t
}

func TestDerivedNames(t *testing.T) {
	assert.Equal(t, "acme-credentials", CredentialsSecretName("acme"))
	assert.Equal(t, "acme", WorkloadName("acme"))
}

func TestMutateNamespaceSetsGuardRailLabels(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}}
	mutateNamespace(testTenant(func(x *tenancyv1alpha1.Tenant) {
		x.Spec.ContactEmail = "platform@example.com"
	}), ns)

	assert.Equal(t, "acme", ns.Labels[tenancyv1alpha1.LabelTenant])
	assert.Equal(t, tenancyv1alpha1.ManagedByValue, ns.Labels[tenancyv1alpha1.LabelManagedBy])
	assert.Equal(t, "baseline", ns.Labels["pod-security.kubernetes.io/enforce"])
	assert.Equal(t, "platform@example.com", ns.Annotations[tenancyv1alpha1.AnnotationContact])
}

// Reconciliation must not turn into a fight with every other tool that writes
// to the same objects, so anything I do not own is left where it is.
func TestMutateNamespaceKeepsForeignMetadata(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "tenant-acme",
		Labels:      map[string]string{"istio-injection": "enabled"},
		Annotations: map[string]string{"team.example.com/cost-centre": "R&D"},
	}}
	mutateNamespace(testTenant(), ns)

	assert.Equal(t, "enabled", ns.Labels["istio-injection"])
	assert.Equal(t, "R&D", ns.Annotations["team.example.com/cost-centre"])
}

func TestMutateNamespaceClearsTheContactWhenItIsRemoved(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "tenant-acme",
		Annotations: map[string]string{tenancyv1alpha1.AnnotationContact: "old@example.com"},
	}}
	mutateNamespace(testTenant(), ns)
	assert.NotContains(t, ns.Annotations, tenancyv1alpha1.AnnotationContact)
}

func TestMutateResourceQuotaFollowsTheTier(t *testing.T) {
	rq := &corev1.ResourceQuota{}
	mutateResourceQuota(testTenant(func(x *tenancyv1alpha1.Tenant) {
		x.Spec.Tier = tenancyv1alpha1.TierEnterprise
	}), rq)

	assert.Equal(t, tenancyv1alpha1.PolicyFor(tenancyv1alpha1.TierEnterprise).QuotaHard(), rq.Spec.Hard)
	assert.Equal(t, "enterprise", rq.Labels[tenancyv1alpha1.LabelTier])
}

func TestMutateNetworkPolicyIsolatesTheTenant(t *testing.T) {
	np := &networkingv1.NetworkPolicy{}
	mutateNetworkPolicy(testTenant(), np)

	require.Len(t, np.Spec.PolicyTypes, 1)
	assert.Equal(t, networkingv1.PolicyTypeIngress, np.Spec.PolicyTypes[0])
	assert.Empty(t, np.Spec.PodSelector.MatchLabels, "the policy should cover every pod in the namespace")

	require.Len(t, np.Spec.Ingress, 1)
	peers := np.Spec.Ingress[0].From
	require.Len(t, peers, 2, "only the tenant's own namespace and the ingress namespace may reach it")
	assert.Equal(t, "acme", peers[0].NamespaceSelector.MatchLabels[tenancyv1alpha1.LabelTenant])
	assert.Equal(t, "ingress-nginx", peers[1].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
}

func TestMutateSecretGeneratesAKeyOnce(t *testing.T) {
	s := &corev1.Secret{}
	require.NoError(t, mutateSecret(testTenant(), s))

	first := string(s.Data[apiKeyField])
	require.NotEmpty(t, first)
	assert.Equal(t, corev1.SecretTypeOpaque, s.Type)

	// Rotating the credential on every reconcile would pull it out from under
	// a running workload every time anything about the tenant changed.
	require.NoError(t, mutateSecret(testTenant(func(x *tenancyv1alpha1.Tenant) {
		x.Spec.Tier = tenancyv1alpha1.TierEnterprise
	}), s))
	assert.Equal(t, first, string(s.Data[apiKeyField]), "the api key must survive a reconcile")
	assert.Equal(t, "enterprise", s.Labels[tenancyv1alpha1.LabelTier], "but the labels should still converge")
}

func TestMutateSecretKeysAreNotPredictable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		s := &corev1.Secret{}
		require.NoError(t, mutateSecret(testTenant(), s))
		key := string(s.Data[apiKeyField])
		assert.False(t, seen[key], "generated the same api key twice")
		assert.GreaterOrEqual(t, len(key), 40)
		seen[key] = true
	}
}

func TestMutateSecretPropagatesAGeneratorFailure(t *testing.T) {
	original := GenerateAPIKey
	defer func() { GenerateAPIKey = original }()
	GenerateAPIKey = func() (string, error) { return "", assert.AnError }

	err := mutateSecret(testTenant(), &corev1.Secret{})
	assert.ErrorIs(t, err, assert.AnError)
}

func TestMutateDeployment(t *testing.T) {
	d := &appsv1.Deployment{}
	mutateDeployment(testTenant(), d)

	require.NotNil(t, d.Spec.Replicas)
	assert.Equal(t, int32(2), *d.Spec.Replicas)
	require.Len(t, d.Spec.Template.Spec.Containers, 1)

	c := d.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "ghcr.io/example/app:1.4.2", c.Image)
	assert.Equal(t, int32(workloadPort), c.Ports[0].ContainerPort)
	assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop)
	assert.True(t, *d.Spec.Template.Spec.SecurityContext.RunAsNonRoot)
	assert.False(t, *d.Spec.Template.Spec.AutomountServiceAccountToken)

	var apiKeyEnv *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "TENANT_API_KEY" {
			apiKeyEnv = &c.Env[i]
		}
	}
	require.NotNil(t, apiKeyEnv, "the workload should read its key from the secret, not from the spec")
	assert.Equal(t, "acme-credentials", apiKeyEnv.ValueFrom.SecretKeyRef.Name)

	// The pod template must be selectable by the immutable selector.
	for k, v := range d.Spec.Selector.MatchLabels {
		assert.Equal(t, v, d.Spec.Template.Labels[k], "pods would not match the selector on %q", k)
	}
}

// A read-only root filesystem is only a hardening measure if the container can
// still start. Without a writable mount the sample workload this repo ships
// crash-loops on its first write, so the mount is part of the contract, not a
// detail of one image.
func TestMutateDeploymentGivesTheWorkloadAWritableScratchDir(t *testing.T) {
	for _, tier := range tenancyv1alpha1.KnownTiers() {
		tenant := testTenant()
		tenant.Spec.Tier = tier

		d := &appsv1.Deployment{}
		mutateDeployment(tenant, d)

		require.Len(t, d.Spec.Template.Spec.Volumes, 1, "%s: expected exactly one scratch volume", tier)
		vol := d.Spec.Template.Spec.Volumes[0]
		assert.Equal(t, scratchVolumeName, vol.Name)
		require.NotNil(t, vol.EmptyDir, "%s: scratch must be an emptyDir", tier)

		// An unbounded emptyDir is node ephemeral storage the namespace quota
		// does not reach, so the tier's cap has to actually land on it.
		require.NotNil(t, vol.EmptyDir.SizeLimit, "%s: scratch volume is unbounded", tier)
		expected := tenancyv1alpha1.PolicyFor(tier).ScratchSizeLimit()
		assert.True(t, expected.Equal(*vol.EmptyDir.SizeLimit),
			"%s: scratch size limit is %s, want %s", tier, vol.EmptyDir.SizeLimit, &expected)

		c := d.Spec.Template.Spec.Containers[0]
		require.True(t, *c.SecurityContext.ReadOnlyRootFilesystem, "%s", tier)
		require.Len(t, c.VolumeMounts, 1, "%s: the scratch volume is declared but not mounted", tier)
		assert.Equal(t, scratchVolumeName, c.VolumeMounts[0].Name)
		assert.Equal(t, scratchMountPath, c.VolumeMounts[0].MountPath)
	}
}

func TestMutateDeploymentSuspends(t *testing.T) {
	d := &appsv1.Deployment{}
	mutateDeployment(testTenant(func(x *tenancyv1alpha1.Tenant) { x.Spec.Suspended = true }), d)
	assert.Equal(t, int32(0), *d.Spec.Replicas)
}

// The selector is immutable in the API, so a tier change must not rewrite it.
func TestMutateDeploymentNeverRewritesTheSelector(t *testing.T) {
	d := &appsv1.Deployment{}
	mutateDeployment(testTenant(), d)
	original := d.Spec.Selector.DeepCopy()

	mutateDeployment(testTenant(func(x *tenancyv1alpha1.Tenant) {
		x.Spec.Tier = tenancyv1alpha1.TierEnterprise
	}), d)
	assert.Equal(t, original, d.Spec.Selector)
}

func TestMutateService(t *testing.T) {
	svc := &corev1.Service{}
	mutateService(testTenant(), svc)

	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, tenancyv1alpha1.SelectorLabels("acme"), svc.Spec.Selector)
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, int32(80), svc.Spec.Ports[0].Port)
	assert.Equal(t, int32(workloadPort), svc.Spec.Ports[0].TargetPort.IntVal)
}

func TestMergeLabelsHandlesANilMap(t *testing.T) {
	got := mergeLabels(nil, map[string]string{"a": "b"})
	assert.Equal(t, map[string]string{"a": "b"}, got)
}
