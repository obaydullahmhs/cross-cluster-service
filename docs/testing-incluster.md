# Testing the remote path with one cluster

`InCluster` points a `RemoteCluster` at **the cluster the controller is already
running in**. That sounds pointless, and for production it is. As a test it is
the most useful thing in the project.

A `CrossService` with a `clusterRef` takes a completely different code path from
one without: it goes through the RemoteCluster grant check, the credential
builder, the reference-counted client cache, and a separate set of informers.
`InCluster` exercises every one of those against your own apiserver, with no
second cluster, no credentials to copy, no firewall rule, and no VPC peering.

If this works and a real secondary cluster does not, the problem is networking
or credentials -- not the controller. That is the whole point of running it.

---

## Prerequisites

**It must be deployed, not run locally.** `InCluster` calls
`rest.InClusterConfig()`, which needs the ServiceAccount token and the
`KUBERNETES_SERVICE_HOST` environment that only exist inside a Pod. Under
`make run` it fails immediately, and says so:

```
unable to load in-cluster configuration, KUBERNETES_SERVICE_HOST and
KUBERNETES_SERVICE_PORT must be defined
```

That is a correct failure, not a bug. Deploy first:

```bash
make install
make deploy IMG=<your-image>
kubectl -n cross-cluster-service-system get pods
```

**RBAC is already in place.** The controller's ClusterRole grants
`get`/`list`/`watch` on pods, nodes, services and endpointslices, which is
exactly what a source needs. Nothing extra to bind.

---

## 1. Declare the cluster

```bash
kubectl apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: RemoteCluster
metadata:
  name: self
spec:
  displayName: this cluster, reached the long way round
  access:
    type: InCluster
    inCluster: {}
  # Fails closed. Omit this and no namespace may use the cluster at all,
  # which is the single most common surprise when testing.
  allowedNamespaces:
    matchNames: [default]
EOF
```

```bash
kubectl get remotecluster self
```

```
NAME   TYPE        READY   VERSION    AGE
self   InCluster   True    v1.31.1    2s
```

`READY=True` already proves a lot: credentials resolved, a `rest.Config` was
built, and a discovery call to the apiserver succeeded. `VERSION` is the real
answer from `/version`, not a placeholder.

If it is not `True`:

```bash
kubectl describe remotecluster self
```

The `Authenticated` and `Reachable` conditions separate "could not build
credentials" from "built them but could not connect".

---

## 2. Use it from a CrossService

Something to point at:

```bash
kubectl create deployment web --image=nginx --replicas=3
```

Now the same Pods source you would write for a real secondary cluster, with a
`clusterRef` added:

```bash
kubectl apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: web-viaremote
  namespace: default
spec:
  ports:
    - port: 80
      targetPort: 80
  source:
    type: Pods
    clusterRef: {name: self}      # <- forces the remote code path
    pods:
      namespace: default
      selector:
        matchLabels: {app: web}
EOF
```

```bash
kubectl get crossservice web-viaremote
```

```
NAME            SERVICE         READY   TOTAL   STATUS   AGE
web-viaremote   web-viaremote   3       3       True     2s
```

Confirm it actually routes:

```bash
kubectl run -it --rm probe --image=curlimages/curl --restart=Never -- \
  curl -s -o /dev/null -w '%{http_code}\n' http://web-viaremote
```

---

## 3. Check the things only this path exercises

**Reference counting.** The RemoteCluster tracks how many CrossServices hold a
reference to its cached client:

```bash
kubectl get remotecluster self -o jsonpath='{.status.consumerCount}{"\n"}'
```

Expect `1`. Create a second CrossService using `clusterRef: {name: self}` and it
becomes `2`; delete one and it drops back. Zero means the cached client and its
informers were torn down.

**Watches over the remote client.** Remote sources are informer-driven, so this
should be effectively instant rather than waiting on any interval:

```bash
kubectl scale deployment web --replicas=6
kubectl get crossservice web-viaremote -w
```

**`allowedNamespaces` failing closed.** The most valuable thing to verify,
because it is a security property rather than a feature:

```bash
kubectl create ns not-granted
kubectl apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: denied
  namespace: not-granted
spec:
  ports: [{port: 80}]
  source:
    type: Pods
    clusterRef: {name: self}
    pods:
      namespace: default
      selector:
        matchLabels: {app: web}
EOF

kubectl -n not-granted describe crossservice denied | grep -A3 Conditions
kubectl -n not-granted get events --field-selector reason=ClusterAccessDenied
```

The CR is accepted -- it is valid -- but it never resolves, and the reason says
exactly why:

```
Ready   False   ClusterAccessDenied
  namespace "not-granted" is not permitted to reference RemoteCluster "self"
```

`not-granted` was never listed in `allowedNamespaces`, so it gets nothing. Add
it to `matchNames` and the same object starts working with no other change.

---

## 4. Exercise the Service source too

Worth doing, because `via: NodePort` is the path where the controller reads a
number you never typed:

```bash
kubectl expose deployment web --type=NodePort --port=80 --name=web-np
kubectl get svc web-np -o jsonpath='{.spec.ports[0].nodePort}{"\n"}'   # note it

kubectl apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: web-nodeport
  namespace: default
spec:
  ports:
    - port: 8080
  source:
    type: Service
    clusterRef: {name: self}
    service:
      namespace: default
      name: web-np
      via: NodePort
EOF

kubectl get endpointslice -l kubernetes.io/service-name=web-nodeport \
  -o jsonpath='{.items[*].ports[*].port}{"\n"}'
```

The port printed there should match the `nodePort` you noted -- and nothing in
the CrossService names it. Delete and recreate `web-np` to get a different
nodePort, and the endpoints follow on their own.

---

## What this does and does not prove

Proven:

- RemoteCluster reconcile, conditions and version probing
- the credential builder and `rest.Config` construction
- the reference-counted client cache and its informers
- `allowedNamespaces` enforcement
- the resolvers running against a client they did not create
- Service, EndpointSlice, port joining and packing

Not proven, because there is no second cluster involved:

- network reachability, authorized IP ranges, VPC peering, private DNS
- credentials from a Secret, and their rotation
- TLS against a CA you supplied
- that `targetRef` is correctly omitted for genuinely remote Pods

So `InCluster` working tells you the controller is sound. It does not tell you
your networking is.

---

## Caveats

`InCluster` opens a **second** set of informers against the same apiserver --
the manager already has one. Harmless for testing, wasteful in production. There
is no reason to use it outside of validating this path.

It also ignores `access.tls`, `proxy`, and any credential fields: it uses the
Pod's own ServiceAccount, whatever that is. If you want to test impersonation or
a CA bundle, use `Token` against a real cluster.

---

## Cleaning up

```bash
kubectl delete crossservice web-viaremote web-nodeport -n default
kubectl -n not-granted delete crossservice denied
kubectl delete ns not-granted
kubectl delete remotecluster self
kubectl delete deployment web
kubectl delete svc web-np
```

Generated Services and EndpointSlices are owned by their CrossService, so they
go with it.
