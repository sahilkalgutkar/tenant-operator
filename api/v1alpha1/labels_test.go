package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOwnedLabels(t *testing.T) {
	got := OwnedLabels("acme", TierEnterprise)
	assert.Equal(t, "acme", got[LabelTenant])
	assert.Equal(t, "enterprise", got[LabelTier])
	assert.Equal(t, ManagedByValue, got[LabelManagedBy])
}

// The tier can change over a tenant's life and a Deployment's selector cannot,
// so the tier must never leak into the selector — an upgrade would otherwise
// produce a Deployment update the API server rejects.
func TestSelectorLabelsExcludeAnythingMutable(t *testing.T) {
	selector := SelectorLabels("acme")
	assert.NotContains(t, selector, LabelTier)

	owned := OwnedLabels("acme", TierFree)
	for k, v := range selector {
		assert.Equal(t, v, owned[k], "selector label %q must agree with the owned labels", k)
	}
}
