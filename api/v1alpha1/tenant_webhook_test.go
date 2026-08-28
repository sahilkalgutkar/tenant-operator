package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func tenant(name string, mutate ...func(*Tenant)) *Tenant {
	t := &Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: TenantSpec{
			DisplayName: strings.ToTitle(name),
			Tier:        TierStandard,
			Image:       "ghcr.io/example/app:1.4.2",
		},
	}
	for _, m := range mutate {
		m(t)
	}
	return t
}

func TestApplyDefaults(t *testing.T) {
	subject := &Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	ApplyDefaults(subject)

	assert.Equal(t, TierStandard, subject.Spec.Tier, "a tenant with no tier should land on standard")
	assert.Equal(t, "tenant-acme", subject.Spec.Namespace)
	require.NotNil(t, subject.Spec.Replicas)
	assert.Equal(t, PolicyFor(TierStandard).DefaultReplicas, *subject.Spec.Replicas)
	assert.Equal(t, "standard", subject.Labels[LabelTier])
}

func TestApplyDefaultsLeavesExplicitValuesAlone(t *testing.T) {
	three := int32(3)
	subject := tenant("acme", func(x *Tenant) {
		x.Spec.Tier = TierEnterprise
		x.Spec.Namespace = "acme-prod"
		x.Spec.Replicas = &three
	})
	ApplyDefaults(subject)

	assert.Equal(t, TierEnterprise, subject.Spec.Tier)
	assert.Equal(t, "acme-prod", subject.Spec.Namespace)
	assert.Equal(t, int32(3), *subject.Spec.Replicas)
}

// Defaulting is idempotent because the controller calls it too, on an object
// the webhook has already been through.
func TestApplyDefaultsIsIdempotent(t *testing.T) {
	subject := &Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	ApplyDefaults(subject)
	first := subject.DeepCopy()
	ApplyDefaults(subject)
	assert.Equal(t, first, subject)
}

func TestDefaulterRejectsTheWrongType(t *testing.T) {
	err := (&TenantDefaulter{}).Default(context.Background(), &corev1.Pod{})
	assert.ErrorContains(t, err, "expected a Tenant")
}

func TestDefaulterDefaultsThroughTheWebhookEntrypoint(t *testing.T) {
	subject := &Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	require.NoError(t, (&TenantDefaulter{}).Default(context.Background(), subject))
	assert.Equal(t, TierStandard, subject.Spec.Tier)
}

func TestValidateImage(t *testing.T) {
	cases := []struct {
		image   string
		wantErr string
	}{
		{"ghcr.io/example/app:1.4.2", ""},
		{"app:v2", ""},
		{"registry:5000/example/app:1.0.0", ""},
		{"example/app@sha256:" + strings.Repeat("a", 64), ""},
		{"", "an image is required"},
		{"ghcr.io/example/app", "explicit tag or digest"},
		{"registry:5000/example/app", "explicit tag or digest"},
		{"ghcr.io/example/app:", "empty tag"},
		{"ghcr.io/example/app:latest", "must not use the :latest tag"},
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			errs := validateImage(tc.image)
			if tc.wantErr == "" {
				assert.Empty(t, errs, "expected %q to be accepted", tc.image)
				return
			}
			require.Len(t, errs, 1)
			assert.Contains(t, errs[0].Error(), tc.wantErr)
		})
	}
}

func TestValidateCreate(t *testing.T) {
	v := &TenantValidator{}
	ctx := context.Background()

	cases := []struct {
		name    string
		subject *Tenant
		wantErr string
	}{
		{"a well-formed tenant", tenant("acme"), ""},
		{
			"a name that is not a DNS label",
			tenant("Acme Corp"),
			"metadata.name",
		},
		{
			"a name too long to fit a namespace",
			tenant(strings.Repeat("a", maxTenantNameLength+1)),
			"metadata.name",
		},
		{
			"more replicas than the tier allows",
			tenant("acme", func(x *Tenant) {
				x.Spec.Tier = TierFree
				n := int32(5)
				x.Spec.Replicas = &n
			}),
			"exceeds the maximum of 1 for the free tier",
		},
		{
			"a reserved namespace",
			tenant("acme", func(x *Tenant) { x.Spec.Namespace = "kube-system" }),
			"reserved namespace",
		},
		{
			"a namespace that is not a DNS label",
			tenant("acme", func(x *Tenant) { x.Spec.Namespace = "Acme_Prod" }),
			"spec.namespace",
		},
		{
			"a floating image tag",
			tenant("acme", func(x *Tenant) { x.Spec.Image = "nginx:latest" }),
			"must not use the :latest tag",
		},
		{
			"a contact that is not an address",
			tenant("acme", func(x *Tenant) { x.Spec.ContactEmail = "platform-team" }),
			"must be an email address",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.ValidateCreate(ctx, tc.subject)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateCreateRejectsTheWrongType(t *testing.T) {
	_, err := (&TenantValidator{}).ValidateCreate(context.Background(), &corev1.Pod{})
	assert.ErrorContains(t, err, "expected a Tenant")
}

// Moving the namespace would orphan every object already created for the
// tenant and provision a second, empty copy alongside it.
func TestValidateUpdateRejectsANamespaceMove(t *testing.T) {
	old := tenant("acme", func(x *Tenant) { x.Spec.Namespace = "tenant-acme" })
	updated := tenant("acme", func(x *Tenant) { x.Spec.Namespace = "tenant-acme-v2" })

	_, err := (&TenantValidator{}).ValidateUpdate(context.Background(), old, updated)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespace is immutable")
}

func TestValidateUpdateAllowsATierChange(t *testing.T) {
	old := tenant("acme", func(x *Tenant) { x.Spec.Namespace = "tenant-acme" })
	updated := tenant("acme", func(x *Tenant) {
		x.Spec.Namespace = "tenant-acme"
		x.Spec.Tier = TierEnterprise
	})

	_, err := (&TenantValidator{}).ValidateUpdate(context.Background(), old, updated)
	assert.NoError(t, err)
}

// A downgrade that would leave the tenant above its new ceiling has to be
// rejected: clamping it silently would take replicas away from a running
// tenant without anybody asking for that.
func TestValidateUpdateRejectsADowngradeThatBreachesTheNewCeiling(t *testing.T) {
	five := int32(5)
	old := tenant("acme", func(x *Tenant) {
		x.Spec.Namespace = "tenant-acme"
		x.Spec.Replicas = &five
	})
	updated := old.DeepCopy()
	updated.Spec.Tier = TierFree

	_, err := (&TenantValidator{}).ValidateUpdate(context.Background(), old, updated)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds the maximum of 1 for the free tier")
}

func TestValidateUpdateRejectsTheWrongTypes(t *testing.T) {
	v := &TenantValidator{}
	_, err := v.ValidateUpdate(context.Background(), &corev1.Pod{}, tenant("acme"))
	assert.ErrorContains(t, err, "expected a Tenant")

	_, err = v.ValidateUpdate(context.Background(), tenant("acme"), &corev1.Pod{})
	assert.ErrorContains(t, err, "expected a Tenant")
}

func TestValidateDelete(t *testing.T) {
	v := &TenantValidator{}
	ctx := context.Background()

	t.Run("an unprotected tier deletes freely", func(t *testing.T) {
		_, err := v.ValidateDelete(ctx, tenant("acme"))
		assert.NoError(t, err)
	})

	t.Run("an enterprise tenant needs confirmation", func(t *testing.T) {
		subject := tenant("acme", func(x *Tenant) { x.Spec.Tier = TierEnterprise })
		_, err := v.ValidateDelete(ctx, subject)
		require.Error(t, err)
		assert.Contains(t, err.Error(), AnnotationConfirmDelete)
	})

	t.Run("the confirmation annotation unlocks it", func(t *testing.T) {
		subject := tenant("acme", func(x *Tenant) {
			x.Spec.Tier = TierEnterprise
			x.Annotations = map[string]string{AnnotationConfirmDelete: "true"}
		})
		_, err := v.ValidateDelete(ctx, subject)
		assert.NoError(t, err)
	})

	t.Run("any other annotation value is not confirmation", func(t *testing.T) {
		subject := tenant("acme", func(x *Tenant) {
			x.Spec.Tier = TierEnterprise
			x.Annotations = map[string]string{AnnotationConfirmDelete: "yes"}
		})
		_, err := v.ValidateDelete(ctx, subject)
		assert.Error(t, err)
	})

	t.Run("the wrong type is rejected", func(t *testing.T) {
		_, err := v.ValidateDelete(ctx, &corev1.Pod{})
		assert.ErrorContains(t, err, "expected a Tenant")
	})
}

func TestWarnings(t *testing.T) {
	one := int32(1)

	noContact, err := (&TenantValidator{}).ValidateCreate(context.Background(), tenant("acme"))
	require.NoError(t, err)
	assert.Contains(t, strings.Join(noContact, " "), "contactEmail is empty")

	suspended := tenant("acme", func(x *Tenant) {
		x.Spec.ContactEmail = "platform@example.com"
		x.Spec.Suspended = true
	})
	warns, err := (&TenantValidator{}).ValidateCreate(context.Background(), suspended)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(warns, " "), "scaled to zero")

	single := tenant("acme", func(x *Tenant) {
		x.Spec.ContactEmail = "platform@example.com"
		x.Spec.Tier = TierFree
		x.Spec.Replicas = &one
	})
	warns, err = (&TenantValidator{}).ValidateCreate(context.Background(), single)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(warns, " "), "single replica")

	quiet := tenant("acme", func(x *Tenant) { x.Spec.ContactEmail = "platform@example.com" })
	warns, err = (&TenantValidator{}).ValidateCreate(context.Background(), quiet)
	require.NoError(t, err)
	assert.Empty(t, warns)
}
