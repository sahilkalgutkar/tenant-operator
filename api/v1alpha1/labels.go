package v1alpha1

// Labels I stamp on every object I create. They are what makes a tenant's
// footprint greppable: `kubectl get all -A -l tenancy.sahilkalgutkar.io/tenant=acme`
// returns exactly what this operator owns for that tenant and nothing else.
const (
	LabelTenant    = "tenancy.sahilkalgutkar.io/tenant"
	LabelTier      = "tenancy.sahilkalgutkar.io/tier"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelName      = "app.kubernetes.io/name"

	// ManagedByValue is the value I put in the managed-by label. Anything
	// carrying it is mine to reconcile, and anything not carrying it I leave
	// alone even if it is sitting in a tenant namespace.
	ManagedByValue = "tenant-operator"
)

// AnnotationContact carries the tenant's contact address onto the namespace,
// where namespace-scoped alert routing can pick it up.
const AnnotationContact = "tenancy.sahilkalgutkar.io/contact"

// OwnedLabels returns the label set I stamp on every object belonging to a
// tenant.
func OwnedLabels(tenant string, tier TenantTier) map[string]string {
	return map[string]string{
		LabelTenant:    tenant,
		LabelTier:      string(tier),
		LabelManagedBy: ManagedByValue,
		LabelName:      tenant,
	}
}

// SelectorLabels returns the subset of labels that a Deployment's selector uses.
// This is deliberately smaller than OwnedLabels: a Deployment's selector is
// immutable, so it must not include anything that can change over a tenant's
// life — the tier can be upgraded, and if it were in the selector that upgrade
// would become an unappliable update.
func SelectorLabels(tenant string) map[string]string {
	return map[string]string{
		LabelTenant:    tenant,
		LabelManagedBy: ManagedByValue,
	}
}
