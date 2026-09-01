package controller

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
)

// The names I derive for a tenant's objects. They are deterministic functions
// of the tenant name so that a reconcile after a controller restart finds what
// the previous one created instead of making a second copy.
const (
	credentialsSecretSuffix = "-credentials"
	quotaName               = "tenant-quota"
	networkPolicyName       = "tenant-isolation"
	apiKeyField             = "api-key"
	workloadPort            = 8080
	scratchVolumeName       = "scratch"
	scratchMountPath        = "/tmp"
)

// CredentialsSecretName is the Secret holding a tenant's generated API key.
func CredentialsSecretName(tenant string) string { return tenant + credentialsSecretSuffix }

// WorkloadName is the Deployment and Service name for a tenant.
func WorkloadName(tenant string) string { return tenant }

// GenerateAPIKey produces a tenant's API key. It is a package-level variable so
// tests can make it deterministic; nothing else replaces it.
var GenerateAPIKey = func() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// mutateNamespace shapes the tenant's namespace. I only ever write the labels
// and annotations I own — anything else somebody has put on the namespace
// survives a reconcile, because stomping unrelated metadata would make this
// controller hostile to every other tool in the cluster.
func mutateNamespace(t *tenancyv1alpha1.Tenant, ns *corev1.Namespace) {
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	for k, v := range tenancyv1alpha1.OwnedLabels(t.Name, t.Spec.Tier) {
		ns.Labels[k] = v
	}
	// The pod-security label is not optional in my mind: a tenant namespace
	// that allows privileged pods is a tenant that can read every other
	// tenant's data off the node.
	ns.Labels["pod-security.kubernetes.io/enforce"] = "baseline"
	ns.Labels["pod-security.kubernetes.io/audit"] = "restricted"

	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	if t.Spec.ContactEmail != "" {
		ns.Annotations[tenancyv1alpha1.AnnotationContact] = t.Spec.ContactEmail
	} else {
		delete(ns.Annotations, tenancyv1alpha1.AnnotationContact)
	}
}

// mutateResourceQuota derives the namespace quota from the tier.
func mutateResourceQuota(t *tenancyv1alpha1.Tenant, rq *corev1.ResourceQuota) {
	rq.Labels = mergeLabels(rq.Labels, tenancyv1alpha1.OwnedLabels(t.Name, t.Spec.Tier))
	rq.Spec.Hard = tenancyv1alpha1.PolicyFor(t.Spec.Tier).QuotaHard()
}

// mutateNetworkPolicy writes a default-deny policy with two holes in it:
// traffic from inside the tenant's own namespace, and traffic from whatever
// namespace the ingress controller runs in. Everything else — including every
// other tenant — is denied, which is the property that makes these namespaces
// a tenancy boundary rather than just a naming convention.
func mutateNetworkPolicy(t *tenancyv1alpha1.Tenant, np *networkingv1.NetworkPolicy) {
	np.Labels = mergeLabels(np.Labels, tenancyv1alpha1.OwnedLabels(t.Name, t.Spec.Tier))
	np.Spec = networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							tenancyv1alpha1.LabelTenant: t.Name,
						},
					},
				},
				{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": "ingress-nginx",
						},
					},
				},
			},
		}},
	}
}

// mutateSecret creates the tenant's API key exactly once. Regenerating it on
// every reconcile would technically converge, but it would also rotate the
// credential out from under a running workload every time anything about the
// tenant changed, which is a worse outcome than the drift it would be fixing.
func mutateSecret(t *tenancyv1alpha1.Tenant, s *corev1.Secret) error {
	s.Labels = mergeLabels(s.Labels, tenancyv1alpha1.OwnedLabels(t.Name, t.Spec.Tier))
	if s.Type == "" {
		s.Type = corev1.SecretTypeOpaque
	}
	if len(s.Data[apiKeyField]) > 0 {
		return nil
	}
	key, err := GenerateAPIKey()
	if err != nil {
		return err
	}
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	s.Data[apiKeyField] = []byte(key)
	return nil
}

// mutateDeployment shapes the tenant's workload. Replicas come from
// EffectiveReplicas rather than straight from the spec, so suspension and the
// tier ceiling are applied in exactly one place.
func mutateDeployment(t *tenancyv1alpha1.Tenant, d *appsv1.Deployment) {
	owned := tenancyv1alpha1.OwnedLabels(t.Name, t.Spec.Tier)
	selector := tenancyv1alpha1.SelectorLabels(t.Name)
	replicas := t.Spec.EffectiveReplicas()
	policy := tenancyv1alpha1.PolicyFor(t.Spec.Tier)

	d.Labels = mergeLabels(d.Labels, owned)
	d.Spec.Replicas = &replicas
	// The selector is immutable once the Deployment exists, so I only ever set
	// it on the way in and never touch it again.
	if d.Spec.Selector == nil {
		d.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
	}

	podLabels := mergeLabels(map[string]string{}, owned)
	podLabels[tenancyv1alpha1.LabelComponent] = "workload"

	d.Spec.Template.ObjectMeta.Labels = podLabels
	d.Spec.Template.Spec.AutomountServiceAccountToken = ptr(false)
	d.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot: ptr(true),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	// A read-only root filesystem with nowhere to write is not a hardened
	// container, it is a container that does not start: almost every image
	// wants a scratch directory, and the ones built for read-only rootfs --
	// nginx-unprivileged, the sample this repo ships -- are written to put
	// their temp paths under /tmp precisely because that is where the writable
	// mount is expected to be. So I keep the root filesystem read-only and
	// hand the workload one bounded emptyDir instead.
	scratchLimit := policy.ScratchSizeLimit()
	d.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name: scratchVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &scratchLimit},
		},
	}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{
		Name:      "workload",
		Image:     t.Spec.Image,
		Resources: policy.ResourceRequirements(),
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: workloadPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      scratchVolumeName,
			MountPath: scratchMountPath,
		}},
		Env: []corev1.EnvVar{
			{Name: "TENANT_NAME", Value: t.Name},
			{Name: "TENANT_TIER", Value: string(t.Spec.Tier)},
			{
				Name: "TENANT_API_KEY",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: CredentialsSecretName(t.Name),
						},
						Key: apiKeyField,
					},
				},
			},
		},
	}}
}

// mutateService fronts the tenant's pods.
func mutateService(t *tenancyv1alpha1.Tenant, svc *corev1.Service) {
	svc.Labels = mergeLabels(svc.Labels, tenancyv1alpha1.OwnedLabels(t.Name, t.Spec.Tier))
	svc.Spec.Selector = tenancyv1alpha1.SelectorLabels(t.Name)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Spec.Ports = []corev1.ServicePort{{
		Name:       "http",
		Port:       80,
		TargetPort: intstr.FromInt32(workloadPort),
		Protocol:   corev1.ProtocolTCP,
	}}
}

// mergeLabels writes src over dst without dropping keys somebody else added.
func mergeLabels(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func ptr[T any](v T) *T { return &v }
