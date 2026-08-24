# cross-cluster-service

Reach a Service in another Kubernetes cluster as if it were local.

```yaml
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: payments
  namespace: storefront
spec:
  ports:
    - name: http
      port: 80
  source:
    type: Service
    clusterRef: { name: prod-eu }
    service:
      namespace: payments
      name: api
      via: PodIP
```

Your application then calls `payments.storefront.svc.cluster.local` — ordinary
cluster DNS, ordinary `ClusterIP`, ordinary kube-proxy load balancing. It has no
idea the backends are running somewhere else.

---

## The problem

A Kubernetes Service finds its backends with a label selector, and a label
selector can only see Pods in its own cluster. The moment the thing you need is
somewhere else — another cluster, another cloud, a VM, a managed database — that
mechanism stops working, and you are left choosing between:

- **hardcoding an IP** into config, and hand-editing it every time it changes
- **a second ingress hop**, paying for a load balancer and a TLS round trip to
  reach something already inside your network
- **a service mesh**, which solves this and also asks you to adopt sidecars, a
  new control plane, and mTLS everywhere first

None of those are wrong, but all three are large answers to a small question:
*what are the current IPs of that thing, and can my Service point at them?*

## The approach

Kubernetes already separates the two halves of a Service. The `Service` object
is the stable name and virtual IP; the `EndpointSlice` objects are the actual
backend addresses. Normally the endpoints controller fills the second in from the
selector — but if you create a Service with **no selector**, nothing does, and
you may write them yourself.

That is the whole idea. This controller keeps EndpointSlices accurate for
backends the selector cannot see:

```
   another cluster                    your cluster
   ┌──────────────┐                  ┌────────────────────────────┐
   │ Pods, Nodes, │   read-only      │  CrossService  (your CR)   │
   │ Services     │ ───────────────► │       ↓                    │
   └──────────────┘   watch          │  Service (no selector)     │
                                     │  EndpointSlice ← written   │
                                     │       ↓                    │
                                     │  kube-proxy → real traffic │
                                     └────────────────────────────┘
```

Nothing sits in the data path. No sidecar, no proxy, no extra hop. Once the
EndpointSlice is written, packets take exactly the route they would to a local
Service, because to kube-proxy that is what it is.

---

## What it can point at

One `CrossService` has one `source`. The interesting one is `Service`, because it
reads values the remote cluster allocates and you cannot know in advance.

| Source | What it resolves | Use it for |
|---|---|---|
| `Service` + `via: PodIP` | the remote Service's Pod IPs | flat networking — VPC-native clusters on one network |
| `Service` + `via: NodePort` | node IPs + the **allocated** nodePort | clusters that can reach each other's nodes but not Pods |
| `Service` + `via: LoadBalancer` | the provider-assigned LB address | clusters connected only through a load balancer |
| `Pods` | Pod IPs, by label selector or name | workloads with no Service in front of them |
| `Nodes` | node addresses | a DaemonSet on a hostPort, a node agent |
| `DNS` | A / AAAA / SRV records | anything outside Kubernetes — a managed database, a VM, a legacy host |
| `Static` | literal IPs | fixed appliances, test fixtures |

`via: NodePort` is the one that repays reading twice. A nodePort is allocated by
the remote cluster from the 30000-32767 range and changes if the Service is
recreated; a LoadBalancer address is assigned by the cloud provider. Writing
either into config by hand means hand-syncing it forever. The controller reads
them across the cluster boundary and keeps them current.

## How it stays current

Watches, not polling. Changes in a secondary cluster reach the local
EndpointSlice in **well under a second** — measured against two live clusters:

| Event in the secondary cluster | Local EndpointSlice | Latency |
|---|---|---|
| Pod deleted | address removed | 0.15s |
| Replacement Pod ready | address added | 0.1s |
| Node joins | node IP added | 5s |
| Node removed | node IP removed | 1s |
| Node goes NotReady | endpoint marked not-ready | as fast as Kubernetes marks it |

A periodic resync (`resyncPeriod`, default 10m) exists only as a backstop for a
lost watch event. It is not the update path, and if you ever see endpoints
changing on a 10-minute boundary rather than instantly, something is wrong.

DNS sources are the exception: they poll, because DNS has no watch. They honour
either a fixed `interval` or the record's TTL.

---

## Connecting to another cluster

Credentials live in a cluster-scoped `RemoteCluster`, so a namespace owner never
handles them:

```yaml
apiVersion: net.obaydullah.dev/v1alpha1
kind: RemoteCluster
metadata:
  name: prod-eu
spec:
  access:
    type: Token
    token:
      server: https://10.0.0.1
      secretRef: { name: prod-eu-creds, key: token }
    tls:
      caSecretRef: { name: prod-eu-creds, key: ca.crt }
  allowedNamespaces:
    matchNames: ["storefront"]     # nil means NONE — this fails closed
```

**Implemented today:** `InCluster`, `Token`, `Kubeconfig`, `GoogleToken`,
`WorkloadIdentity`, `ClientCertificate`, `GKE`, `AKS`.

**Declared but not yet implemented** — the CRD accepts them and the controller
reports a clear condition rather than failing obscurely: `ConnectGateway`,
`ProjectedServiceAccount`, `ExecPlugin`, `EKS`.

If you are starting out, use `Token`: a ServiceAccount in the target cluster
bound to a read-only ClusterRole. It involves no cloud IAM, so it sidesteps an
entire category of problem, and it is the least privilege that can work.

---

## Security model

Cross-cluster credentials deserve more suspicion than most configuration, so
three properties are structural rather than advisory.

**Access to another cluster is read-only, always.** The controller only ever
issues `get`, `list` and `watch`. It never writes to a cluster it reads from, so
a compromised controller cannot use its remote credential to change anything.

**Credentials resolve from exactly one namespace.** `secretRef` has *no*
`namespace` field, and Secrets are cached from the controller's own namespace
only — enforced in the informer, not just filtered after the fact, so the
controller's RBAC for Secrets is a namespaced `Role` and never a `ClusterRole`.
Without this, a cluster-scoped `RemoteCluster` naming any Secret in the cluster
would be a credential-exfiltration primitive.

**Namespace access fails closed.** `allowedNamespaces` is opt-in: omit it and
*no* namespace may reference that cluster. Remote Pod IPs are sensitive, and a
cluster-scoped object is not a namespace owner's to reason about.

Beyond that, an `addressPolicy` filters what may be written — link-local,
loopback and multicast are rejected by default, which is what stops a
`CrossService` from pointing a Service at `169.254.169.254` and handing out the
node's cloud metadata credentials.

---

## Installing

From source — there is no tagged release yet:

```bash
git clone https://github.com/obaydullahmhs/cross-cluster-service
cd cross-cluster-service

make install                                    # CRDs
make deploy IMG=<registry>/cross-cluster-service:tag
```

Then follow **[docs/getting-started.md](docs/getting-started.md)**, which walks
through a first CrossService and covers GKE and AKS specifics.

To try it end to end on your laptop, **[docs/testing-two-k3s.md](docs/testing-two-k3s.md)**
builds two real clusters in Docker and exercises every source type. Every command
in it has been run, with output quoted inline.

If `via: PodIP` is what you want but Pod IPs are not routable between your
clusters, **[docs/pod-ip-connectivity.md](docs/pod-ip-connectivity.md)** covers
establishing that with Submariner — and, first, how to tell whether you need it.

---

## Operating it

```console
$ kubectl get crossservice -n storefront
NAME       SERVICE    READY   TOTAL   STATUS   AGE
payments   payments   3       3       True     4m
```

`READY`/`TOTAL` are endpoint counts, so a partial outage is visible at a glance.
Conditions distinguish the cases that matter: `SourcesResolved` (could we read
the source), `EndpointsWritten` (did we write), `Degraded` (are we serving stale
data), `Ready` (is this usable).

`failurePolicy` decides what happens when a source becomes unreadable. The
default, `MarkNotReady`, keeps the endpoints and flags them — a brief apiserver
blip should not black-hole a working Service. `Remove` deletes them instead.

One thing to be explicit about: **nothing here health-checks backends.** A
perfectly fresh EndpointSlice can point at an address nothing can reach, if the
network between the clusters does not allow it. This controller answers *where
is it*, not *is it up*.

---

## What this is not

- **Not a service mesh.** No sidecars, no mTLS, no traffic policy. If you need
  those, use a mesh; this solves one small part of what a mesh does.
- **Not a network.** It does not create connectivity between clusters. Pod IPs
  must already be routable for `via: PodIP` to carry traffic; otherwise use
  `NodePort` or `LoadBalancer`. Endpoints will be *correct* either way, which is
  a useful distinction when debugging.
- **Not a DNS system.** It uses the cluster DNS you already have.
- **Not multi-cluster failover.** One `CrossService` has one source.

## Related work

The Kubernetes SIG standard for this problem is
[MCS-API](https://github.com/kubernetes/enhancements/tree/master/keps/sig-multicluster/1645-multi-cluster-services-api)
(`ServiceExport` / `ServiceImport`), implemented by Submariner and others.
Linkerd's service-mirror and Istio's multicluster support solve overlapping
problems inside a mesh.

This project is deliberately smaller: no broker, no mesh, no agent in the
secondary cluster — a read-only credential and a controller. That is a poor
trade if you want the rest of what a mesh gives you, and a good one if you want
a Service that points somewhere else and nothing more.

---

## Status

**v1alpha1.** The API may change. It is tested against two live clusters —
resolution, datapath, failure and recovery, credential rotation, RBAC scoping —
and used in anger, but it has not been through a wide range of environments yet.
Issues and reports from your setup are genuinely useful.

## Contributing

`make test` runs unit and envtest suites; `make lint` must be clean. The
invariants the controller must hold are named in the test suite after the thing
they protect, so a test called `TestI7_...` failing tells you which property you
broke.

## License

Apache 2.0 — every source file carries the header. A top-level `LICENSE` file
still needs adding; take the canonical text from
[apache.org/licenses/LICENSE-2.0.txt](https://www.apache.org/licenses/LICENSE-2.0.txt).
