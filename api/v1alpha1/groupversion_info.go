// Package v1alpha1 contains the Tenant API I serve from this operator.
//
// +kubebuilder:object:generate=true
// +groupName=tenancy.sahilkalgutkar.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version this API is served under.
	GroupVersion = schema.GroupVersion{Group: "tenancy.sahilkalgutkar.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types in this package with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this package to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
