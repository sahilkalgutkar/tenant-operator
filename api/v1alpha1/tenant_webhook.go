package v1alpha1

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// maxTenantNameLength keeps "tenant-<name>" inside the 63-character limit a
// namespace name has, with room to spare. I would rather reject a long name at
// admission than have the controller fail to create a namespace later, where
// the error is buried in a condition instead of returned to whoever ran apply.
const maxTenantNameLength = 40

// reservedNamespaces are namespaces a tenant may never be pointed at. Adopting
// kube-system into a tenant would mean the tenant's finalizer deletes it on
// teardown, which is a cluster-ending outcome from a one-line typo.
var reservedNamespaces = map[string]bool{
	"default":         true,
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

// +kubebuilder:webhook:path=/mutate-tenancy-sahilkalgutkar-io-v1alpha1-tenant,mutating=true,failurePolicy=fail,sideEffects=None,groups=tenancy.sahilkalgutkar.io,resources=tenants,verbs=create;update,versions=v1alpha1,name=mtenant.kb.io,admissionReviewVersions=v1

// The webhook handlers are stateless plumbing, not API objects, so they are
// excluded from deepcopy generation.
//
// +kubebuilder:object:generate=false

// TenantDefaulter fills in the parts of a Tenant a human should not have to
// write out. Defaulting lives in a webhook rather than in the controller so
// that what comes back from `kubectl get` is what the controller will act on —
// a spec that is quietly reinterpreted at reconcile time is one a person cannot
// review.
type TenantDefaulter struct{}

var _ admission.CustomDefaulter = &TenantDefaulter{}

func (d *TenantDefaulter) Default(_ context.Context, obj runtime.Object) error {
	t, ok := obj.(*Tenant)
	if !ok {
		return fmt.Errorf("expected a Tenant, got %T", obj)
	}
	ApplyDefaults(t)
	return nil
}

// ApplyDefaults is the defaulting logic itself, separated from the webhook
// plumbing so it can be called directly from tests and from the controller's
// own bootstrap path.
func ApplyDefaults(t *Tenant) {
	if t.Spec.Tier == "" {
		t.Spec.Tier = TierStandard
	}
	if t.Spec.Namespace == "" {
		t.Spec.Namespace = DefaultNamespaceFor(t.Name)
	}
	if t.Spec.Replicas == nil {
		r := PolicyFor(t.Spec.Tier).DefaultReplicas
		t.Spec.Replicas = &r
	}
	if t.Labels == nil {
		t.Labels = map[string]string{}
	}
	// Labelling the Tenant with its own tier is redundant with the spec, but
	// it makes `kubectl get tenants -l tenancy.sahilkalgutkar.io/tier=free`
	// work, and label selectors are what people actually reach for.
	t.Labels[LabelTier] = string(t.Spec.Tier)
}

// +kubebuilder:webhook:path=/validate-tenancy-sahilkalgutkar-io-v1alpha1-tenant,mutating=false,failurePolicy=fail,sideEffects=None,groups=tenancy.sahilkalgutkar.io,resources=tenants,verbs=create;update;delete,versions=v1alpha1,name=vtenant.kb.io,admissionReviewVersions=v1

// +kubebuilder:object:generate=false

// TenantValidator rejects tenants that the controller could only fail on.
type TenantValidator struct{}

var _ admission.CustomValidator = &TenantValidator{}

func (v *TenantValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	t, ok := obj.(*Tenant)
	if !ok {
		return nil, fmt.Errorf("expected a Tenant, got %T", obj)
	}
	if errs := validateTenant(t); len(errs) > 0 {
		return warningsFor(t), apierrors.NewInvalid(GroupVersion.WithKind("Tenant").GroupKind(), t.Name, errs)
	}
	return warningsFor(t), nil
}

func (v *TenantValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldT, ok := oldObj.(*Tenant)
	if !ok {
		return nil, fmt.Errorf("expected a Tenant, got %T", oldObj)
	}
	newT, ok := newObj.(*Tenant)
	if !ok {
		return nil, fmt.Errorf("expected a Tenant, got %T", newObj)
	}

	errs := validateTenant(newT)

	// The namespace is the one field I cannot let move. Every object I have
	// created for this tenant lives in the old namespace; repointing the spec
	// would orphan all of them and provision a second, empty copy alongside.
	if oldT.NamespaceName() != newT.NamespaceName() {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "namespace"), newT.Spec.Namespace,
			fmt.Sprintf("namespace is immutable (currently %q); delete and recreate the tenant to move it", oldT.NamespaceName()),
		))
	}

	if len(errs) > 0 {
		return warningsFor(newT), apierrors.NewInvalid(GroupVersion.WithKind("Tenant").GroupKind(), newT.Name, errs)
	}
	return warningsFor(newT), nil
}

func (v *TenantValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	t, ok := obj.(*Tenant)
	if !ok {
		return nil, fmt.Errorf("expected a Tenant, got %T", obj)
	}
	// Deleting a tenant deletes its namespace, and with it whatever the tenant
	// was storing. For the tiers where that is somebody's production data, I
	// want a second deliberate action rather than a single command.
	if PolicyFor(t.Spec.Tier).Protected && t.Annotations[AnnotationConfirmDelete] != "true" {
		return nil, apierrors.NewForbidden(
			GroupVersion.WithResource("tenants").GroupResource(), t.Name,
			fmt.Errorf("tenant is on the %s tier: set the %s=true annotation to confirm you intend to destroy its namespace", t.Spec.Tier, AnnotationConfirmDelete),
		)
	}
	return nil, nil
}

// validateTenant holds every rule that applies on both create and update.
func validateTenant(t *Tenant) field.ErrorList {
	var errs field.ErrorList

	if len(t.Name) > maxTenantNameLength {
		errs = append(errs, field.TooLong(field.NewPath("metadata", "name"), "", maxTenantNameLength))
	}
	for _, msg := range validation.IsDNS1123Label(t.Name) {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), t.Name, msg))
	}

	ns := t.NamespaceName()
	for _, msg := range validation.IsDNS1123Label(ns) {
		errs = append(errs, field.Invalid(field.NewPath("spec", "namespace"), ns, msg))
	}
	if reservedNamespaces[ns] {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "namespace"), ns,
			"is a reserved namespace; the tenant finalizer would delete it on teardown",
		))
	}

	errs = append(errs, validateImage(t.Spec.Image)...)

	policy := PolicyFor(t.Spec.Tier)
	if t.Spec.Replicas != nil && *t.Spec.Replicas > policy.MaxReplicas {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "replicas"), *t.Spec.Replicas,
			fmt.Sprintf("exceeds the maximum of %d for the %s tier", policy.MaxReplicas, t.Spec.Tier),
		))
	}

	if t.Spec.ContactEmail != "" && !strings.Contains(t.Spec.ContactEmail, "@") {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "contactEmail"), t.Spec.ContactEmail, "must be an email address",
		))
	}

	return errs
}

// validateImage insists on an immutable image reference. A floating tag like
// :latest breaks the one property this controller depends on: that the spec
// describes what is running. With a mutable tag, two pods created from the
// identical Deployment can be running different code, and no amount of
// reconciliation will notice or correct it.
func validateImage(image string) field.ErrorList {
	var errs field.ErrorList
	path := field.NewPath("spec", "image")

	if image == "" {
		return append(errs, field.Required(path, "an image is required"))
	}
	if strings.Contains(image, "@sha256:") {
		return errs
	}

	// Strip any registry host before looking for a tag, so that a port in
	// "registry:5000/app" is not mistaken for one.
	ref := image
	if i := strings.LastIndex(image, "/"); i >= 0 {
		ref = image[i+1:]
	}
	tagIdx := strings.LastIndex(ref, ":")
	if tagIdx < 0 {
		return append(errs, field.Invalid(path, image, "must specify an explicit tag or digest"))
	}
	tag := ref[tagIdx+1:]
	if tag == "" {
		return append(errs, field.Invalid(path, image, "has an empty tag"))
	}
	if tag == "latest" {
		return append(errs, field.Invalid(path, image,
			"must not use the :latest tag: a mutable tag means the spec no longer describes what is running"))
	}
	return errs
}

// warningsFor returns advisory messages that do not block admission. These are
// the things I want a human to see at apply time but would not stop a rollout
// for.
func warningsFor(t *Tenant) admission.Warnings {
	var w admission.Warnings
	if t.Spec.ContactEmail == "" {
		w = append(w, "spec.contactEmail is empty: alerts for this tenant will fall back to the platform team's default route")
	}
	if t.Spec.Suspended {
		w = append(w, "spec.suspended is true: the tenant's workload will be scaled to zero, but its namespace and data are retained")
	}
	if t.Spec.Tier == TierFree && t.Spec.Replicas != nil && *t.Spec.Replicas == 1 {
		w = append(w, "free tier runs a single replica: expect downtime during rollouts")
	}
	return w
}

// SetupTenantWebhookWithManager registers both webhooks with the manager.
func SetupTenantWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&Tenant{}).
		WithDefaulter(&TenantDefaulter{}).
		WithValidator(&TenantValidator{}).
		Complete()
}

var _ webhook.CustomValidator = &TenantValidator{}
