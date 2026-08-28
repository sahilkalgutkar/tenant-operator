# Operating the tenant operator

Everything here is what I would want to know at 3am, not a restatement of the
README.

## Flags

The operator takes no environment variables of its own — everything is a flag,
so `kubectl get deploy -o yaml` shows the entire configuration of a running
instance without cross-referencing a ConfigMap.

| Flag | Default | Notes |
| --- | --- | --- |
| `--metrics-bind-address` | `:8080` | controller-runtime's Prometheus endpoint, at `/metrics`. `0` disables it. |
| `--health-probe-bind-address` | `:8081` | Serves `/healthz` and `/readyz`. Required when leader election is on. |
| `--leader-elect` | `true` | Two managers reconciling the same Tenant would fight over every object they both own. Turn it off only for a local run against a throwaway cluster. |
| `--webhook-port` | `9443` | |
| `--webhook-cert-dir` | `/tmp/k8s-webhook-server/serving-certs` | Must contain `tls.crt` and `tls.key`. |
| `--enable-webhooks` | `true` | Off for `make run`, since a local process has no certificate the API server would trust. |

Kubernetes credentials come from the usual places: the in-cluster service
account when it is running as a pod, `$KUBECONFIG` or `~/.kube/config` when it
is not.

## What it needs from the cluster

- **Cluster-wide** on namespaces (it creates one per tenant) and on the
  `tenants` custom resource.
- **Namespaced, in tenant namespaces**: deployments, services, secrets,
  resource quotas and network policies.
- **In its own namespace**: leases, for leader election.

The generated `config/rbac/role.yaml` is the authoritative list — it is
produced from the `+kubebuilder:rbac` markers on the reconciler, so it cannot
drift away from what the code actually does.

## Certificates

The webhooks need a certificate the API server trusts. The install manifests
ask cert-manager for one and rely on its CA injector to write the bundle into
both webhook configurations:

```
Issuer (self-signed) → Certificate → Secret webhook-server-cert → mounted at --webhook-cert-dir
                                  ↘ cert-manager.io/inject-ca-from annotation → webhook configs
```

If cert-manager is not installed, the webhook configurations have no CA bundle,
every `Tenant` create fails closed, and the message you get back is a TLS error
rather than anything about certificates. That failure mode is correct — a
validating webhook that fails open is not validating anything — but it is
confusing if you were not expecting it. `--enable-webhooks=false` plus deleting
the two webhook configurations is the way to run without them.

## Reading a Tenant that is unhappy

`status.conditions` is where the answer is. `Ready` is a roll-up, so start with
the component conditions underneath it:

| Condition | False means |
| --- | --- |
| `NamespaceReady` | The namespace, quota or network policy could not be created or converged. The most common cause is the operator refusing to adopt an object it did not create — the message names it. |
| `WorkloadReady` | The Deployment exists but has fewer ready replicas than the tenant asked for. This is also the normal state during a rollout. |

```bash
kubectl get tenant globex -o jsonpath='{.status.conditions}' | jq
kubectl describe tenant globex          # the events tell the same story chronologically
kubectl get events -n tenant-globex --sort-by=.lastTimestamp
```

`status.observedGeneration` against `metadata.generation` tells you whether the
status you are reading reflects the spec you are reading. If they differ, the
controller has not caught up with the last edit yet.

## Things that look like bugs and are not

**A tenant sits in `Provisioning` forever.** Its Deployment has fewer ready
replicas than it wants. Look at the pods: an unschedulable pod, an image the
cluster cannot pull, or a quota the tenant has already filled will all park it
here. The operator is correctly reporting that the tenant is not up.

**`kubectl delete tenant` hangs.** That is the finalizer doing its job. The
namespace is still terminating, usually because something inside it has a
finalizer of its own. `kubectl get ns tenant-<name> -o yaml` shows what.

**A hand-edited Deployment reverts.** Also intended. Change the `Tenant`
instead; the Deployment is derived state.

**Deleting an enterprise tenant is rejected.** Annotate it to confirm:

```bash
kubectl annotate tenant globex tenancy.sahilkalgutkar.io/confirm-delete=true
```

## Metrics worth alerting on

controller-runtime exports these for free; these are the three I would page on:

- `controller_runtime_reconcile_errors_total{controller="tenant"}` — a nonzero
  rate means tenants are not converging.
- `workqueue_depth{name="tenant"}` — a depth that does not drain means the
  controller is stuck or badly outnumbered.
- `controller_runtime_reconcile_time_seconds` — a p99 that climbs is usually
  the API server, not the operator.
