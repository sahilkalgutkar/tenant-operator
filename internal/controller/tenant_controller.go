package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	tenancyv1alpha1 "github.com/sahilkalgutkar/tenant-operator/api/v1alpha1"
)

// errStatusConflict means somebody else wrote the Tenant while I was working
// on it. It is not a failure — it is the normal outcome of optimistic
// concurrency under a busy workqueue — so I requeue on it rather than logging
// a reconcile error and backing off, which would fill the operator's logs with
// noise that reads like something is broken.
var errStatusConflict = errors.New("the tenant was modified while the status was being written")

// resyncInterval is a safety net, not the mechanism. Watches on the owned
// objects are what actually drive this controller; the periodic resync only
// exists to catch the things a watch cannot tell me about, such as a namespace
// finishing its termination.
const resyncInterval = 30 * time.Second

// TenantReconciler drives a Tenant towards the cluster state it describes.
type TenantReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=tenancy.sahilkalgutkar.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tenancy.sahilkalgutkar.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tenancy.sahilkalgutkar.io,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas;secrets;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the whole control loop. It is written to be safe to run at any
// time, from any starting state, including halfway through a previous run that
// crashed — every step is an idempotent "make this look like that", and the
// function never assumes it is the one that created what it finds.
func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var tenant tenancyv1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		// A NotFound here means the Tenant is gone and the finalizer already
		// ran. There is nothing left to do and nothing to report.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tenant.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &tenant)
	}

	if !controllerutil.ContainsFinalizer(&tenant, tenancyv1alpha1.Finalizer) {
		controllerutil.AddFinalizer(&tenant, tenancyv1alpha1.Finalizer)
		if err := r.Update(ctx, &tenant); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// The update bumps the resource version; letting the resulting watch
		// event drive the next pass keeps this function working from a single
		// consistent read of the object.
		return ctrl.Result{Requeue: true}, nil
	}

	// The webhook normally fills these in, but the controller must not depend
	// on the webhook being installed: a cluster where the webhook is
	// temporarily unavailable should still converge, not reconcile a spec with
	// an empty tier against a nil replica count.
	tenancyv1alpha1.ApplyDefaults(&tenant)

	if err := r.reconcileNamespace(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, tenancyv1alpha1.ConditionNamespaceReady, "NamespaceError", err)
	}
	if err := r.reconcileGuardRails(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, tenancyv1alpha1.ConditionNamespaceReady, "GuardRailError", err)
	}
	meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type:               tenancyv1alpha1.ConditionNamespaceReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Provisioned",
		Message:            fmt.Sprintf("namespace %s has its quota and network policy in place", tenant.NamespaceName()),
		ObservedGeneration: tenant.Generation,
	})

	if err := r.reconcileWorkload(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, tenancyv1alpha1.ConditionWorkloadReady, "WorkloadError", err)
	}

	if err := r.observeWorkload(ctx, &tenant); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.writeStatus(ctx, &tenant); err != nil {
		if errors.Is(err, errStatusConflict) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	if tenant.Status.Phase != tenancyv1alpha1.PhaseReady && tenant.Status.Phase != tenancyv1alpha1.PhaseSuspended {
		logger.V(1).Info("tenant not settled yet", "phase", tenant.Status.Phase)
		return ctrl.Result{RequeueAfter: resyncInterval}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileNamespace creates or converges the tenant's namespace.
func (r *TenantReconciler) reconcileNamespace(ctx context.Context, t *tenancyv1alpha1.Tenant) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: t.NamespaceName()}}
	op, err := r.apply(ctx, t, ns, func() error {
		mutateNamespace(t, ns)
		return nil
	})
	if err != nil {
		return err
	}
	if op == controllerutil.OperationResultCreated {
		r.event(t, corev1.EventTypeNormal, "NamespaceCreated", fmt.Sprintf("created namespace %s", ns.Name))
	}
	t.Status.Namespace = ns.Name
	return nil
}

// reconcileGuardRails puts the quota and the network policy in place. They are
// reconciled before the workload on purpose: if a tenant's pods came up first,
// there would be a window in which they were running unquota'd and reachable
// from every other namespace.
func (r *TenantReconciler) reconcileGuardRails(ctx context.Context, t *tenancyv1alpha1.Tenant) error {
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: quotaName, Namespace: t.NamespaceName()}}
	if _, err := r.apply(ctx, t, quota, func() error {
		mutateResourceQuota(t, quota)
		return nil
	}); err != nil {
		return fmt.Errorf("resource quota: %w", err)
	}

	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: networkPolicyName, Namespace: t.NamespaceName()}}
	if _, err := r.apply(ctx, t, np, func() error {
		mutateNetworkPolicy(t, np)
		return nil
	}); err != nil {
		return fmt.Errorf("network policy: %w", err)
	}
	return nil
}

// reconcileWorkload converges the credentials Secret, the Deployment and the
// Service.
func (r *TenantReconciler) reconcileWorkload(ctx context.Context, t *tenancyv1alpha1.Tenant) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      CredentialsSecretName(t.Name),
		Namespace: t.NamespaceName(),
	}}
	op, err := r.apply(ctx, t, secret, func() error { return mutateSecret(t, secret) })
	if err != nil {
		return fmt.Errorf("credentials secret: %w", err)
	}
	if op == controllerutil.OperationResultCreated {
		r.event(t, corev1.EventTypeNormal, "CredentialsIssued", "generated the tenant API key")
	}
	t.Status.CredentialsSecret = secret.Name

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      WorkloadName(t.Name),
		Namespace: t.NamespaceName(),
	}}
	if _, err := r.apply(ctx, t, deploy, func() error {
		mutateDeployment(t, deploy)
		return nil
	}); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      WorkloadName(t.Name),
		Namespace: t.NamespaceName(),
	}}
	if _, err := r.apply(ctx, t, svc, func() error {
		mutateService(t, svc)
		return nil
	}); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	return nil
}

// observeWorkload reads the Deployment back and turns what it finds into the
// workload condition. This is the half of the loop that is easy to skip and
// expensive to skip: without it, a Tenant would report Ready the instant its
// Deployment object existed, whether or not a single pod ever started.
func (r *TenantReconciler) observeWorkload(ctx context.Context, t *tenancyv1alpha1.Tenant) error {
	var deploy appsv1.Deployment
	key := types.NamespacedName{Name: WorkloadName(t.Name), Namespace: t.NamespaceName()}
	if err := r.Get(ctx, key, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
				Type:               tenancyv1alpha1.ConditionWorkloadReady,
				Status:             metav1.ConditionFalse,
				Reason:             "DeploymentMissing",
				Message:            "the deployment has not appeared in the cache yet",
				ObservedGeneration: t.Generation,
			})
			return nil
		}
		return fmt.Errorf("reading deployment: %w", err)
	}

	desired := t.Spec.EffectiveReplicas()
	t.Status.DesiredReplicas = desired
	t.Status.ReadyReplicas = deploy.Status.ReadyReplicas

	switch {
	case deploy.Status.ReadyReplicas >= desired:
		reason, msg := "AllReplicasReady", fmt.Sprintf("%d/%d replicas ready", deploy.Status.ReadyReplicas, desired)
		if desired == 0 {
			reason, msg = "ScaledToZero", "tenant is suspended: no replicas are expected"
		}
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               tenancyv1alpha1.ConditionWorkloadReady,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: t.Generation,
		})
	default:
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               tenancyv1alpha1.ConditionWorkloadReady,
			Status:             metav1.ConditionFalse,
			Reason:             "ReplicasNotReady",
			Message:            fmt.Sprintf("%d/%d replicas ready", deploy.Status.ReadyReplicas, desired),
			ObservedGeneration: t.Generation,
		})
	}
	return nil
}

// finalize tears a tenant down in a defined order and, crucially, does not let
// the Tenant object disappear until the namespace has actually gone. Owner
// references alone would delete the same objects, but asynchronously: the
// Tenant would vanish from the API while its namespace was still terminating,
// and anything scripted around `kubectl delete tenant` would race the cleanup.
func (r *TenantReconciler) finalize(ctx context.Context, t *tenancyv1alpha1.Tenant) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(t, tenancyv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}

	var ns corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: t.NamespaceName()}, &ns)
	switch {
	case apierrors.IsNotFound(err):
		// The namespace is gone, so everything inside it went with it.
		controllerutil.RemoveFinalizer(t, tenancyv1alpha1.Finalizer)
		if err := r.Update(ctx, t); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
		}
		r.event(t, corev1.EventTypeNormal, "TenantDeleted", "namespace and all tenant resources are gone")
		return ctrl.Result{}, nil

	case err != nil:
		return ctrl.Result{}, fmt.Errorf("reading namespace during teardown: %w", err)
	}

	// I only delete a namespace this operator owns. If somebody pointed a
	// tenant at a namespace that was already there, deleting it on teardown
	// would destroy work that was never mine to destroy.
	if ns.Labels[tenancyv1alpha1.LabelManagedBy] != tenancyv1alpha1.ManagedByValue {
		r.event(t, corev1.EventTypeWarning, "NamespaceNotOwned",
			fmt.Sprintf("namespace %s is not managed by this operator: leaving it in place", ns.Name))
		controllerutil.RemoveFinalizer(t, tenancyv1alpha1.Finalizer)
		return ctrl.Result{}, r.Update(ctx, t)
	}

	if ns.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, &ns); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting namespace: %w", err)
		}
		r.event(t, corev1.EventTypeNormal, "NamespaceDeleting", fmt.Sprintf("deleting namespace %s", ns.Name))
	}

	// While the namespace drains, the Tenant should say so. A tenant that
	// still reads as Ready or Suspended halfway through its own teardown is
	// actively misleading to anybody watching `kubectl get tenants`.
	if t.Status.Phase != tenancyv1alpha1.PhaseTerminating {
		t.Status.Phase = tenancyv1alpha1.PhaseTerminating
		if err := r.Status().Update(ctx, t); err != nil && !apierrors.IsConflict(err) {
			return ctrl.Result{}, fmt.Errorf("recording the terminating phase: %w", err)
		}
	}

	// Namespace termination is not something a watch on the Tenant will tell
	// me about, so this is one of the few places a timed requeue is the right
	// tool rather than a shortcut.
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

// apply is the single place any object is written. Wrapping CreateOrUpdate
// gives me one spot to enforce two invariants: I never adopt an object I did
// not create, and everything I do create is owned by the Tenant so that
// garbage collection is a backstop under the finalizer.
func (r *TenantReconciler) apply(
	ctx context.Context,
	t *tenancyv1alpha1.Tenant,
	obj client.Object,
	mutate func() error,
) (controllerutil.OperationResult, error) {
	return controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := assertNotForeign(t, obj); err != nil {
			return err
		}
		if err := mutate(); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(t, obj, r.Scheme)
	})
}

// assertNotForeign refuses to take over an object that already exists and was
// not created by this operator for this tenant. Adoption looks convenient right
// up to the first time an operator silently rewrites a namespace somebody else
// was using.
func assertNotForeign(t *tenancyv1alpha1.Tenant, obj client.Object) error {
	if obj.GetUID() == "" {
		return nil // brand new, nothing to conflict with
	}
	labels := obj.GetLabels()
	if labels[tenancyv1alpha1.LabelManagedBy] != tenancyv1alpha1.ManagedByValue {
		return fmt.Errorf(
			"%s %s/%s already exists and is not managed by %s: refusing to adopt it",
			obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(),
			tenancyv1alpha1.ManagedByValue)
	}
	if owner := labels[tenancyv1alpha1.LabelTenant]; owner != "" && owner != t.Name {
		return fmt.Errorf(
			"%s/%s belongs to tenant %q: refusing to take it over for tenant %q",
			obj.GetNamespace(), obj.GetName(), owner, t.Name)
	}
	return nil
}

// failWith records a failure on a condition, writes the status and returns the
// error so the workqueue backs off. Reporting the failure on the object matters
// as much as returning it: an error that only ever reaches the controller's log
// is invisible to whoever owns the Tenant.
func (r *TenantReconciler) failWith(
	ctx context.Context,
	t *tenancyv1alpha1.Tenant,
	condType, reason string,
	cause error,
) (ctrl.Result, error) {
	meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		ObservedGeneration: t.Generation,
	})
	r.event(t, corev1.EventTypeWarning, reason, cause.Error())
	if err := r.writeStatus(ctx, t); err != nil {
		log.FromContext(ctx).Error(err, "could not record the failure on the tenant status")
	}
	return ctrl.Result{}, cause
}

// writeStatus recomputes the roll-up condition and phase, then persists them.
func (r *TenantReconciler) writeStatus(ctx context.Context, t *tenancyv1alpha1.Tenant) error {
	ready := readyCondition(t)
	ready.ObservedGeneration = t.Generation
	meta.SetStatusCondition(&t.Status.Conditions, ready)

	t.Status.Phase = phaseFor(t)
	t.Status.ObservedGeneration = t.Generation

	if err := r.Status().Update(ctx, t); err != nil {
		if apierrors.IsConflict(err) {
			return errStatusConflict
		}
		return fmt.Errorf("updating tenant status: %w", err)
	}
	return nil
}

func (r *TenantReconciler) event(t *tenancyv1alpha1.Tenant, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(t, eventType, reason, message)
}

// SetupWithManager wires the controller up to its watches. Owning the objects
// I create is what turns this into an actual control loop: delete a tenant's
// Deployment by hand and the watch fires, the Tenant reconciles, and the
// Deployment comes back.
func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tenancyv1alpha1.Tenant{}).
		Owns(&corev1.Namespace{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&appsv1.Deployment{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 3}).
		Named("tenant").
		Complete(r)
}
