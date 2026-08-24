# Testing with two k3s clusters

A local two-cluster setup for exercising the controller end to end, using k3d
(k3s in Docker). Two genuinely separate clusters, two separate apiservers, two
separate credential sets — the same shape you will have on GKE, without the
cloud.

## What this does and does not prove

**Proves:** `type: Token` authentication, every source type's resolution logic,
EndpointSlice contents and invariants, RBAC scoping, credential rotation,
failure handling when the remote cluster goes away.

**Does not prove:** anything GKE-specific — Workload Identity, node pool OAuth
scopes, VPC routing, multi-zone node selection. See [Appendix C](#appendix-c-what-changes-on-gke).

One thing to be clear about up front: this controller's job is to produce
*correct EndpointSlices*. Whether packets then flow is kube-proxy's job and the
network's. The two are separable, and this document tests them separately —
NodePort for the datapath, Pod IPs for resolution correctness.

---

## 0. Prerequisites

Docker is already running (OrbStack counts). You need k3d:

```bash
brew install k3d
```

---

## 1. Two clusters on one network

The shared Docker network is what makes the clusters mutually reachable.

```bash
docker network create xcluster-net --subnet 172.28.0.0/16
```

**alpha** — where the controller runs:

```bash
k3d cluster create alpha \
  --network xcluster-net \
  --k3s-arg "--cluster-cidr=10.42.0.0/16@server:*" \
  --k3s-arg "--service-cidr=10.43.0.0/16@server:*" \
  --k3s-arg "--disable=traefik@server:*"
```

**beta** — the remote cluster being read:

```bash
k3d cluster create beta \
  --network xcluster-net \
  --k3s-arg "--cluster-cidr=10.44.0.0/16@server:*" \
  --k3s-arg "--service-cidr=10.45.0.0/16@server:*" \
  --k3s-arg "--disable=traefik@server:*"
```

The CIDRs must not overlap. This mirrors the GKE setup (`10.32.0.0/14`,
`10.60.0.0/14`, `10.116.0.0/14`) and is a hard requirement for any Pod-IP-based
source: two clusters using the same Pod CIDR cannot address each other's Pods
even in principle.

Contexts are `k3d-alpha` and `k3d-beta`.

```bash
kubectl --context k3d-alpha get nodes
kubectl --context k3d-beta  get nodes
```

---

## 2. Find beta's address as alpha sees it

This is the step that most often goes wrong.

```bash
BETA_IP=$(docker inspect k3d-beta-server-0 \
  --format '{{.NetworkSettings.Networks."xcluster-net".IPAddress}}')
BETA_API="https://${BETA_IP}:6443"
echo "$BETA_API"
```

**Do not use the `server:` URL from beta's kubeconfig.** k3d points it at
`127.0.0.1:<random-port>`, which is the *host-side* port mapping. From inside a
Pod in alpha, `127.0.0.1` is that Pod. It will fail in a way that looks like a
network problem and is not.

Sanity check from inside alpha:

```bash
kubectl --context k3d-alpha run netcheck --rm -it --restart=Never \
  --image=curlimages/curl -- \
  curl -sk "${BETA_API}/version"
```

A JSON version blob means routing works. A 401 also means routing works — you
just have no credentials yet, which is the next step.

---

## 3. A read-only identity in beta

```bash
kubectl --context k3d-beta apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata: { name: xcluster-reader, namespace: kube-system }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: { name: xcluster-reader }
rules:
  - apiGroups: [""]
    resources: ["pods", "nodes", "services"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: xcluster-reader }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: xcluster-reader }
subjects:
  - { kind: ServiceAccount, name: xcluster-reader, namespace: kube-system }
---
apiVersion: v1
kind: Secret
metadata:
  name: xcluster-reader-token
  namespace: kube-system
  annotations:
    kubernetes.io/service-account.name: xcluster-reader
type: kubernetes.io/service-account-token
EOF
```

No write verbs anywhere. §9.7 says remote access is read-only always; enforcing
it at the *target* end means it holds even if the controller misbehaves.

Confirm the token controller populated the Secret:

```bash
kubectl --context k3d-beta -n kube-system get secret xcluster-reader-token \
  -o jsonpath='{.data.token}' | head -c 20; echo '...'
```

---

## 4. Install the credential into alpha

```bash
kubectl --context k3d-alpha create namespace cross-cluster-service-system \
  --dry-run=client -o yaml | kubectl --context k3d-alpha apply -f -

kubectl --context k3d-alpha -n cross-cluster-service-system \
  create secret generic beta-creds \
  --from-literal=token="$(kubectl --context k3d-beta -n kube-system \
      get secret xcluster-reader-token -o jsonpath='{.data.token}' | base64 -d)" \
  --from-literal=ca.crt="$(kubectl --context k3d-beta -n kube-system \
      get secret xcluster-reader-token -o jsonpath='{.data.ca\.crt}' | base64 -d)" \
  --dry-run=client -o yaml | kubectl --context k3d-alpha apply -f -
```

The namespace must be the controller's own. `SecretKeyRef` has no `namespace`
field by design (§9.1) — a cluster-scoped `RemoteCluster` that could name any
namespace's Secret would be a credential-exfiltration primitive.

---

## 5. Deploy the controller into alpha

```bash
make docker-build IMG=controller:latest
k3d image import controller:latest --cluster alpha
kubectl config use-context k3d-alpha
make deploy IMG=controller:latest

kubectl -n cross-cluster-service-system rollout status deploy/cross-cluster-service-controller-manager
kubectl -n cross-cluster-service-system logs -l control-plane=controller-manager -f
```

`k3d image import` is required — k3d does not read your local Docker image
store, and without it the Pod sits in `ErrImagePull` while the image exists
perfectly well on your machine.

Two things to look for in the logs. Neither should appear:

- `POD_NAMESPACE` unset — the Deployment sets it via the downward API; the
  controller exits rather than guessing which namespace holds credentials.
- Any cluster-scoped Secret watch error. Secrets are cached from one namespace
  only ([cmd/main.go](../cmd/main.go)); a cluster-wide watch here would mean the
  scoping regressed.

---

## 6. Register beta

```bash
kubectl --context k3d-alpha apply -f - <<EOF
apiVersion: net.obaydullah.dev/v1alpha1
kind: RemoteCluster
metadata:
  name: beta
spec:
  displayName: "k3d beta"
  access:
    type: Token
    token:
      server: ${BETA_API}
      secretRef: { name: beta-creds, key: token }
    tls:
      caSecretRef: { name: beta-creds, key: ca.crt }
      serverName: kubernetes.default.svc
  allowedNamespaces:
    matchNames: ["demo"]
EOF

kubectl --context k3d-alpha get remotecluster beta -o yaml | tail -30
```

`serverName` is needed because you are connecting by IP and k3s's serving
certificate is issued for names, not for the Docker-assigned address.

`allowedNamespaces` is not optional in practice: omitting it means **none**
(§9.2 fails closed), and every CrossService will be rejected with a condition
saying so. That is the intended behaviour, not a bug — but it does mean you have
to list `demo` before anything below works.

Wait for `Ready=True` before continuing. If it stays false, the condition
message names the cause.

---

## 7. A workload in beta

```bash
kubectl --context k3d-beta create namespace demo

kubectl --context k3d-beta -n demo create deployment web \
  --image=nginx:alpine --replicas=3
kubectl --context k3d-beta -n demo expose deployment web \
  --port=80 --target-port=80 --type=NodePort --name=web

kubectl --context k3d-beta -n demo get svc web -o wide
kubectl --context k3d-beta -n demo get pods -o wide
```

Note the allocated nodePort and the Pod IPs (`10.44.x.x`) — you will check both
against what the controller writes.

---

## 8. The datapath test: NodePort

This is the path that genuinely carries traffic between two k3d clusters, because
node IPs are Docker addresses on the shared network.

```bash
kubectl --context k3d-alpha create namespace demo

kubectl --context k3d-alpha apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: beta-web
  namespace: demo
spec:
  ports:
    - name: http
      port: 80
  source:
    type: Service
    clusterRef: { name: beta }
    service:
      namespace: demo
      name: web
      via: NodePort
      nodePort:
        addressType: InternalIP
EOF
```

Verify, then actually use it:

```bash
kubectl --context k3d-alpha -n demo get crossservice beta-web -o yaml | tail -25
kubectl --context k3d-alpha -n demo get svc,endpointslice -l \
  endpointslice.kubernetes.io/managed-by=crossservice.net.obaydullah.dev

kubectl --context k3d-alpha -n demo run probe --rm -it --restart=Never \
  --image=curlimages/curl -- curl -s http://beta-web/
```

Nginx's welcome page means the whole chain works: the controller authenticated to
beta, read the Service, discovered the nodePort, wrote an EndpointSlice, and
kube-proxy programmed it.

The EndpointSlice should contain beta's **node** IPs (`172.28.x.x`) paired with
the allocated nodePort — not Pod IPs, and not port 80.

---

## 9. The resolution test: Pod IPs

```bash
kubectl --context k3d-alpha apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata:
  name: beta-web-pods
  namespace: demo
spec:
  ports:
    - name: http
      port: 80
      targetPort: 80
  source:
    type: Pods
    clusterRef: { name: beta }
    pods:
      namespace: demo
      selector:
        matchLabels: { app: web }
EOF

kubectl --context k3d-alpha -n demo get endpointslice \
  -l kubernetes.io/service-name=beta-web-pods -o yaml
```

Check the addresses match beta's Pod IPs exactly, that they update when you scale
beta's Deployment, and that not-ready Pods are marked rather than dropped.

**Traffic will not flow here by default**, and that is expected: the two clusters
run separate flannel overlays, so alpha has no route to `10.44.0.0/16`. The
EndpointSlice is still correct — which is the part this controller is responsible
for. On GKE this same config *does* carry traffic, because VPC-native Pod IPs are
real VPC addresses. [Appendix B](#appendix-b-optional-making-pod-ips-actually-route)
makes it route locally if you want the full path.

---

## 10. The sources that need no remote cluster

```bash
kubectl --context k3d-alpha apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata: { name: static-demo, namespace: demo }
spec:
  ports: [{ port: 8080 }]
  source:
    type: Static
    static:
      addresses: ["10.44.1.10", "10.44.1.11"]
---
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata: { name: dns-demo, namespace: demo }
spec:
  ports: [{ name: https, port: 443 }]
  source:
    type: DNS
    dns:
      names: ["example.com."]
      recordType: A
      interval: 30s
      excludePrivateIPs: true
EOF
```

Worth testing deliberately:

- **Static** — put `169.254.169.254` in the list. It must be rejected with an
  `AddressPolicyRejected` event, not written. That is the metadata-server guard.
- **DNS `excludePrivateIPs`** — flip it to `excludePublicIPs: true` and the slice
  should empty out, with an event naming how many addresses were excluded.
- **Both exclusions true** — the apiserver must reject the object outright.

---

## 11. Failure behaviour

The interesting part is what happens when things break.

```bash
# Remote cluster unreachable: existing endpoints are retained, not dropped.
k3d cluster stop beta
kubectl --context k3d-alpha -n demo get crossservice beta-web -o yaml | tail -20
kubectl --context k3d-alpha -n demo get endpointslice -l kubernetes.io/service-name=beta-web

k3d cluster start beta
```

Default `failurePolicy` is `MarkNotReady` — endpoints stay, flagged not-ready, so
a transient apiserver blip does not blackhole a working Service.

```bash
# Credential rotation: no restart required.
kubectl --context k3d-alpha -n cross-cluster-service-system \
  delete secret beta-creds
# RemoteCluster goes NotReady; recreate it (step 4) and it recovers on its own.
```

```bash
# Namespace gating: this must be refused.
kubectl --context k3d-alpha create namespace other
# ...create a CrossService in `other` referencing beta.
# `demo` is the only allowed namespace, so it fails closed with a clear condition.
```

---

## 12. Keeping a copy of the token

Answering the practical question: yes, a legacy ServiceAccount token can be
backed up and restored.

**Why it works.** The apiserver validates a legacy token by reading the
`secret.name` claim out of the JWT, looking that Secret up, comparing the stored
bytes to the presented token, and checking the ServiceAccount's UID against the
`service-account.uid` claim. Nothing is stateful beyond those objects. Restore
the Secret byte-for-byte and the token is live again.

**The one hard condition:** the ServiceAccount must still exist *with the same
UID*. Delete and recreate the SA and every token ever issued to it is dead —
no backup helps, because the UID baked into the JWT no longer matches.

### Back up

Reconstruct the object rather than dumping it: a raw `get -o yaml` carries
`resourceVersion` and `uid`, which make the restore fail with a conflict later.

```bash
K="kubectl --context k3d-beta -n kube-system"

cat > bin/xcluster-reader-token.backup.yaml <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: xcluster-reader-token
  namespace: kube-system
  annotations:
    kubernetes.io/service-account.name: xcluster-reader
type: kubernetes.io/service-account-token
data:
  token: $($K get secret xcluster-reader-token -o jsonpath='{.data.token}')
  ca.crt: $($K get secret xcluster-reader-token -o jsonpath='{.data.ca\.crt}')
  namespace: $($K get secret xcluster-reader-token -o jsonpath='{.data.namespace}')
EOF

# Record the SA UID too -- without a match, the token is unrestorable.
$K get sa xcluster-reader -o jsonpath='{.metadata.uid}' > bin/xcluster-reader.sa-uid
```

### Restore

```bash
kubectl --context k3d-beta apply -f bin/xcluster-reader-token.backup.yaml
```

Check the UID still matches before trusting it:

```bash
[ "$(kubectl --context k3d-beta -n kube-system get sa xcluster-reader \
     -o jsonpath='{.metadata.uid}')" = "$(cat bin/xcluster-reader.sa-uid)" ] \
  && echo "restorable" || echo "SA was recreated -- issue a new token"
```

### Handle it with care

That backup file is a live, non-expiring credential in plaintext.

- `bin/` is gitignored (`.gitignore:7`), so it will not reach git from there.
  `docs/` is **not** — never put one there.
- Encrypt it if it outlives the afternoon: `age -p -o token.age token.yaml`,
  or sops if you already use it.
- Revoke by deleting the Secret in beta. That invalidates the token immediately,
  including every copy of your backup.

### The better habit

Prefer *regeneration* over restoration. Everything in step 3 is declarative, so
losing the Secret costs one re-apply:

```bash
#!/usr/bin/env bash
# reissue.sh -- recreate the reader token in beta and install it into alpha.
# Expects step 3's manifest saved as xcluster-reader.yaml.
set -euo pipefail

kubectl --context k3d-beta -n kube-system delete secret xcluster-reader-token --ignore-not-found
kubectl --context k3d-beta apply -f xcluster-reader.yaml
kubectl --context k3d-beta -n kube-system wait --for=jsonpath='{.data.token}' \
  secret/xcluster-reader-token --timeout=30s

kubectl --context k3d-alpha -n cross-cluster-service-system \
  create secret generic beta-creds \
  --from-literal=token="$(kubectl --context k3d-beta -n kube-system \
      get secret xcluster-reader-token -o jsonpath='{.data.token}' | base64 -d)" \
  --from-literal=ca.crt="$(kubectl --context k3d-beta -n kube-system \
      get secret xcluster-reader-token -o jsonpath='{.data.ca\.crt}' | base64 -d)" \
  --dry-run=client -o yaml | kubectl --context k3d-alpha apply -f -
```

The controller picks the new credential up on its own — its client cache is keyed
by credential fingerprint, so rewriting the Secret rebuilds the connection with no
restart. A backup you never need beats a backup you have to protect.

---

## Appendix A: teardown

```bash
k3d cluster delete alpha beta
docker network rm xcluster-net
```

---

## Appendix B: optional — making Pod IPs actually route

Only worth doing if you want to exercise the datapath for `Pods` and
`via: PodIP`. Works for single-node clusters, which is what the commands in
step 1 create.

```bash
ALPHA_IP=$(docker inspect k3d-alpha-server-0 \
  --format '{{.NetworkSettings.Networks."xcluster-net".IPAddress}}')
BETA_IP=$(docker inspect k3d-beta-server-0 \
  --format '{{.NetworkSettings.Networks."xcluster-net".IPAddress}}')

docker exec k3d-alpha-server-0 ip route add 10.44.0.0/16 via "$BETA_IP"
docker exec k3d-beta-server-0  ip route add 10.42.0.0/16 via "$ALPHA_IP"
```

Each node then reaches the other's Pod subnet directly, and flannel delivers
locally from there. Re-run step 9's `curl` and it should work.

These routes do not survive a cluster restart.

---

## Appendix C: what changes on GKE

| | k3d | GKE |
|---|---|---|
| Access type | `Token` | `Token` — unchanged |
| Pod IP routing | needs Appendix B | works natively (VPC-native alias IPs) |
| Pod CIDRs | `10.42/16`, `10.44/16` | `10.32/14`, `10.60/14`, `10.116/14` |
| `serverName` | required (connecting by IP) | not needed |
| Firewall | Docker network, open | **verify cross-cluster ingress rules** |
| Token expiry | never (legacy Secret) | same, if you use a legacy Secret |

The one that bites: your GKE Pod CIDRs sit below `10.128.0.0/9`, so the default
VPC's `default-allow-internal` rule does not cover them, and GKE's per-cluster
rules only admit each cluster's own Pod CIDR. Cross-cluster Pod traffic is
likely blocked until you add an ingress rule.

It fails as a **timeout, not an auth error** — and the CrossService will still
report Ready, because the controller verifies the remote *apiserver* is
reachable, not that the backends are. Nothing here health-checks endpoints.
