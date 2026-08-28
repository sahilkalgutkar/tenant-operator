# tenant-operator

A Kubernetes operator that turns "create a tenant" into something the cluster
API understands: a custom resource, a reconcile loop that provisions a
namespace with quota and network isolation, and admission webhooks that catch
bad specs before they become bad conditions.

Work in progress — see the branches.
