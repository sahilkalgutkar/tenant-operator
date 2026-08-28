package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantTier is the service level a tenant is on. The tier is the only knob a
// platform team has to turn: everything expensive about a tenant — how many
// replicas it may run, how much CPU and memory its namespace may consume, how
// many objects it may create — is derived from it, so nobody has to hand-tune
// quotas per customer.
//
// +kubebuilder:validation:Enum=free;standard;enterprise
type TenantTier string

const (
	TierFree       TenantTier = "free"
	TierStandard   TenantTier = "standard"
	TierEnterprise TenantTier = "enterprise"
)

// TenantPhase is a coarse, human-facing summary of where a tenant is. It is
// deliberately derived from the conditions rather than being a separate source
// of truth — conditions are what a controller should reason about, and a phase
// is what a person scanning `kubectl get tenants` wants to see.
//
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Suspended;Degraded;Terminating
type TenantPhase string

const (
	PhasePending      TenantPhase = "Pending"
	PhaseProvisioning TenantPhase = "Provisioning"
	PhaseReady        TenantPhase = "Ready"
	PhaseSuspended    TenantPhase = "Suspended"
	PhaseDegraded     TenantPhase = "Degraded"
	PhaseTerminating  TenantPhase = "Terminating"
)

// Condition types I set on a Tenant. I keep them narrow and orthogonal so each
// one answers exactly one question about the tenant.
const (
	// ConditionReady is the roll-up: the tenant is fully provisioned and its
	// workload has the replicas it is supposed to have.
	ConditionReady = "Ready"
	// ConditionNamespaceReady tracks the tenant's own namespace and the guard
	// rails I put on it (quota, network policy).
	ConditionNamespaceReady = "NamespaceReady"
	// ConditionWorkloadReady tracks the Deployment and Service.
	ConditionWorkloadReady = "WorkloadReady"
)

// Finalizer is the finalizer I put on every Tenant so that `kubectl delete
// tenant` blocks until the namespace has actually gone away. Garbage collection
// through owner references would eventually delete the same objects, but it is
// asynchronous and unordered: the Tenant would disappear from the API while its
// namespace was still terminating, and an operator watching the delete would
// have no way to tell when teardown had finished.
const Finalizer = "tenancy.sahilkalgutkar.io/finalizer"

// AnnotationConfirmDelete must be set on an enterprise Tenant before I will let
// it be deleted. Enterprise tenants are the ones whose namespace holds data
// somebody cares about, and a fat-fingered `kubectl delete tenant` is not a
// recoverable event.
const AnnotationConfirmDelete = "tenancy.sahilkalgutkar.io/confirm-delete"

// TenantSpec is the whole contract a platform team writes against.
type TenantSpec struct {
	// DisplayName is the human-readable name of the customer this tenant is
	// for. It never has to be DNS-safe, which is exactly why it is separate
	// from the object name.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Tier drives the tenant's replica bounds and resource quota.
	//
	// +kubebuilder:default=standard
	Tier TenantTier `json:"tier,omitempty"`

	// Image is the tenant workload's container image. It must carry an
	// explicit tag or digest — see the validating webhook for why a floating
	// tag is rejected rather than merely discouraged.
	//
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Replicas overrides the tier's default replica count. It is still capped
	// by the tier's maximum, so this is a way to run smaller than the tier
	// allows, not a way to escape it.
	//
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Suspended scales the tenant's workload to zero without deleting
	// anything. This is what I want when a customer stops paying: the data,
	// the namespace and the quota all survive, and un-suspending is a one-word
	// edit rather than a re-provision.
	//
	// +kubebuilder:default=false
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// Namespace is the namespace I provision for this tenant. It defaults to
	// "tenant-<name>" and is immutable afterwards: changing it would orphan
	// every object I had already created.
	//
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ContactEmail is who to page about this tenant. I surface it on the
	// namespace as a label-safe annotation so it shows up in alerts routed by
	// namespace rather than by tenant.
	//
	// +optional
	ContactEmail string `json:"contactEmail,omitempty"`
}

// TenantStatus is everything I observe, never anything I am told.
type TenantStatus struct {
	// Phase is the coarse summary shown in `kubectl get tenants`.
	// +optional
	Phase TenantPhase `json:"phase,omitempty"`

	// Conditions are the real state. Ready is the roll-up of the others.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// Without it, a stale Ready=True is indistinguishable from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Namespace is the namespace I actually provisioned.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// CredentialsSecret is the name of the Secret in the tenant's namespace
	// holding its generated API key.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`

	// ReadyReplicas is the number of workload replicas currently ready. It is
	// serialised even when it is zero, so that `kubectl get tenants` shows a
	// real "0" rather than a blank column that reads as missing data.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// DesiredReplicas is what I asked for, after the tier cap and suspension
	// have been applied. Having both numbers on the status means a human can
	// see "0 of 0" (suspended) and "0 of 3" (broken) without going digging.
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tn,categories=tenancy
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tenant is one customer's slice of the cluster: a namespace, the guard rails
// on it, and the workload that runs inside it.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TenantList is a list of Tenants.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
