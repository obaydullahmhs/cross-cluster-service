# Running CrossService in a cluster

Everything here works in a **single cluster** first. Start there: it exercises
the Service, EndpointSlice, port-joining and watch machinery without any
cross-cluster networking to debug. Add a second cluster only once that works.

---

## 0. Prerequisites

```bash
kubectl version --client     # 1.27+
kubectl config current-context
```

You need `cluster-admin` on the cluster you deploy into, because the controller
installs CRDs and a ClusterRole.

---

## 1. Install the CRDs

```bash
make install
kubectl get crd | grep obaydullah
```

Expected:

```
crossservices.net.obaydullah.dev
remoteclusters.net.obaydullah.dev
```

---

## 2. Deploy the controller

Build and push an image your cluster can pull, then deploy:

```bash
export IMG=<your-registry>/cross-cluster-service:v0.1.0

make docker-build docker-push IMG=$IMG
make deploy IMG=$IMG
```

For a local kind cluster you can skip the registry:

```bash
kind create cluster --name crossservice
make docker-build IMG=cross-cluster-service:dev
kind load docker-image cross-cluster-service:dev --name crossservice
make deploy IMG=cross-cluster-service:dev
```

Check it came up:

```bash
kubectl -n cross-cluster-service-system get deploy,pod
kubectl -n cross-cluster-service-system logs deploy/cross-cluster-service-controller-manager -f
```

---

## 3. Smoke test: a Static source

The fastest thing that proves the whole pipeline works.

```bash
kubectl apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: static-demo
spec:
  ports:
    - port: 80
  source:
    type: Static
    static:
      addresses: ["10.20.30.40", "10.20.30.41"]
EOF
```

```bash
kubectl get crossservice static-demo
```

```
NAME          SERVICE       READY   TOTAL   STATUS   AGE
static-demo   static-demo   2       2       True     3s
```

Now look at what it generated. This is the part worth understanding:

```bash
kubectl get svc static-demo -o yaml | grep -A3 'selector\|clusterIP:'
kubectl get endpointslice -l kubernetes.io/service-name=static-demo -o yaml
```

Two things to verify:

- **The Service has no `selector`.** If it had one, the built-in EndpointSlice
  controller would take ownership and delete our slices in a loop.
- **The slice carries `endpointslice.kubernetes.io/managed-by:
  crossservice.net.obaydullah.dev`.** That label is what stops the built-in
  mirroring controller from fighting us for it.

---

## 4. A real one: reach Pods that have no Service

```bash
kubectl create deployment web --image=nginx --replicas=3
kubectl apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: web-direct
spec:
  ports:
    - port: 80
      targetPort: 80
  source:
    type: Pods
    pods:
      namespace: default
      selector:
        matchLabels: {app: web}
EOF
```

Confirm it routes:

```bash
kubectl run -it --rm probe --image=curlimages/curl --restart=Never -- \
  curl -s -o /dev/null -w '%{http_code}\n' http://web-direct
```

Then watch it react. Endpoints are watch-driven, not polled, so this should be
effectively instant rather than taking a poll interval:

```bash
kubectl scale deployment web --replicas=5
kubectl get endpointslice -l kubernetes.io/service-name=web-direct \
  -o jsonpath='{range .items[*]}{.endpoints[*].addresses[0]}{"\n"}{end}'
```

---

## 5. A DNS source

Useful for an external database, or anything not in Kubernetes.

```bash
kubectl apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: external-db
spec:
  ports:
    - name: postgres
      port: 5432
      targetPort: 5432
  source:
    type: DNS
    dns:
      names: ["db.prod.example.com."]   # trailing dot is deliberate
      recordType: A
      interval: 30s
EOF
```

The trailing dot is not cosmetic. Without it, `ndots:5` in a Pod's
`resolv.conf` costs four NXDOMAIN round trips per lookup, per interval,
forever. The controller appends it if you forget, but write it anyway.

---

## 6. Rehearse the remote path without a second cluster

Before wiring up real credentials and firewall rules, point a `RemoteCluster` at
the cluster you are already in. It drives the entire remote code path -- grant
checks, the credential builder, the client cache, separate informers -- with
nothing to configure. If that works and a real secondary cluster does not, the
problem is networking, not the controller.

See [testing-incluster.md](testing-incluster.md).

---

## 7. A second cluster

Do this only after steps 3-6 pass.

### On the cluster being read from

```bash
kubectl create ns crossservice
kubectl create sa remote-reader -n crossservice
kubectl create clusterrole remote-reader \
  --verb=get,list,watch --resource=pods,nodes,services,endpointslices
kubectl create clusterrolebinding remote-reader \
  --clusterrole=remote-reader --serviceaccount=crossservice:remote-reader

kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: remote-reader-token
  namespace: crossservice
  annotations:
    kubernetes.io/service-account.name: remote-reader
type: kubernetes.io/service-account-token
EOF
```

Grab the three things you need:

```bash
kubectl get secret remote-reader-token -n crossservice -o jsonpath='{.data.token}'  | base64 -d > /tmp/token
kubectl get secret remote-reader-token -n crossservice -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/ca.crt
kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}'
```

### On the controller's cluster

Credentials are read from the controller's namespace and **nowhere else**. This
is deliberate: a cluster-scoped CR that could name a Secret in any namespace
would be a credential-exfiltration primitive.

```bash
kubectl -n cross-cluster-service-system create secret generic secondary-token --from-file=token=/tmp/token
kubectl -n cross-cluster-service-system create secret generic secondary-ca    --from-file=ca.crt=/tmp/ca.crt

kubectl apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: RemoteCluster
metadata:
  name: secondary-a
spec:
  access:
    type: Token
    token:
      server: https://<endpoint-from-above>
      secretRef: {name: secondary-token, key: token}
    tls:
      caSecretRef: {name: secondary-ca, key: ca.crt}
  allowedNamespaces:
    matchNames: [default]
EOF
```

`allowedNamespaces` **fails closed**. Omit it and no namespace may use this
cluster at all.

```bash
kubectl get remotecluster secondary-a
```

```
NAME          TYPE    READY   VERSION   AGE
secondary-a   Token   True    v1.31.1   5s
```

If `READY` is not `True`, see the triage table below before going further.

### Consume it

```bash
kubectl apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: remote-web
  namespace: default
spec:
  ports:
    - name: http
      port: 80
  source:
    type: Service
    clusterRef: {name: secondary-a}
    service:
      namespace: default
      name: web
      via: NodePort
EOF
```

Nothing here names a port in the 30000-32767 range. The controller reads the
allocated `nodePort` from the remote Service, so recreating that Service --
which reallocates it -- needs no change on this side.

---

## Triage

Read `status` first; it is written to explain itself.

```bash
kubectl describe crossservice <name>
kubectl describe remotecluster <name>
```

| Symptom | Meaning | Look at |
|---|---|---|
| `timeout` / `connection refused` | never reached the apiserver | authorized networks, VPC peering, egress IP |
| **401** Unauthorized | token rejected | wrong or stale token, wrong CA, wrong endpoint |
| **403** Forbidden | authenticated, RBAC denied | good news -- just add the ClusterRoleBinding |
| DNS resolution failure (AKS) | private DNS zone not linked | link the hub VNet to `privatelink.<region>.azmk8s.io` |
| `ClusterAccessDenied` | namespace not granted | `allowedNamespaces` on the RemoteCluster |
| `AddressPolicyRejected` | an address was dropped | usually link-local; see the Events |
| `NoEndpointsFound` | source resolved to nothing | selector matches no Pods, or named port unresolved |
| `StaleEndpoints` | serving last-known-good | the source has been failing past its threshold |

403 is the friendliest failure: it means authentication worked and only the
binding is missing.

---

## Cloud specifics

**GKE with Workload Identity disabled.** A pod inherits the node's service
account, but that token carries the node pool's OAuth scopes, which by default
do not include `cloud-platform` -- so the GKE control plane rejects it with a
401. Node pool scopes are immutable, so fixing it means a new node pool. The
ServiceAccount-token path above sidesteps this entirely and is why it is the
recommended route.

**AKS private clusters.** The apiserver resolves through a private DNS zone
named `privatelink.<region>.azmk8s.io`. The controller's VNet must be linked to
that zone or the name will not resolve at all -- you will see a DNS failure, not
a TLS or auth error. Alternatives: `--enable-private-cluster-public-fqdn`, or
connect by IP and set `access.tls.serverName` to the real FQDN so SNI and
certificate validation still line up.

**EKS.** Use the ServiceAccount-token path. Native `EKS` access is reserved but
not implemented.

---

## Uninstall

```bash
kubectl delete crossservice --all -A
make undeploy
make uninstall
```

Generated Services and EndpointSlices are owned by their CrossService, so
deleting the CrossService garbage-collects them.
