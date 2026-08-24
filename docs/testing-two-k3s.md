# Testing with two k3s clusters

A local two-cluster setup for exercising the controller end to end: two k3s
servers as plain Docker containers on a shared network. Two separate
apiservers, two separate credential sets — the same shape as the GKE target,
without the cloud and without installing anything on your host.

**Every command here has been run.** Results are quoted inline. Where something
needs care, it is because it actually went wrong the first time.

## What this proves and what it does not

**Proves:** `type: Token` authentication across clusters, every source type's
resolution, EndpointSlice contents, the address policy, namespace gating, RBAC
scoping, failure and recovery behaviour.

**Does not prove:** anything GKE-specific — Workload Identity, node pool OAuth
scopes, VPC routing. See [Appendix C](#appendix-c-what-changes-on-gke).

One distinction the guide leans on throughout: this controller's job is to
produce *correct EndpointSlices*. Whether packets then flow is kube-proxy's job
and the network's. Steps 8 and 9 test those separately.

---

## 0. Prerequisites

Docker (OrbStack is fine) and `kubectl`. Nothing else — no k3d, no VMs.

---

## 1. Two clusters on one network

```bash
docker network create xcs-net --subnet 172.31.0.0/16
docker pull rancher/k3s:latest
```

Static IPs are deliberate: they let the serving certificate carry the address as
a SAN, which removes a whole class of TLS problem later.

**alpha** — where the controller runs:

```bash
docker run -d --name xcs-alpha --privileged --network xcs-net --ip 172.31.0.10 \
  -p 16443:6443 --tmpfs /run --tmpfs /var/run \
  -e K3S_KUBECONFIG_MODE=666 \
  rancher/k3s:latest server \
    --cluster-cidr=10.42.0.0/16 --service-cidr=10.43.0.0/16 \
    --disable=traefik --disable=metrics-server \
    --tls-san=172.31.0.10 --tls-san=127.0.0.1
```

**beta** — the remote cluster being read:

```bash
docker run -d --name xcs-beta --privileged --network xcs-net --ip 172.31.0.20 \
  -p 16444:6443 --tmpfs /run --tmpfs /var/run \
  -e K3S_KUBECONFIG_MODE=666 \
  rancher/k3s:latest server \
    --cluster-cidr=10.44.0.0/16 --service-cidr=10.45.0.0/16 \
    --disable=traefik --disable=metrics-server \
    --tls-san=172.31.0.20 --tls-san=127.0.0.1
```

The Pod CIDRs must not overlap. This mirrors the GKE layout (`10.32.0.0/14`,
`10.60.0.0/14`, `10.116.0.0/14`) and is a hard requirement for any Pod-IP source:
two clusters on the same Pod CIDR cannot address each other's Pods even in
principle.

Both take 30-60s to come up:

```bash
docker exec xcs-alpha kubectl get nodes
docker exec xcs-beta  kubectl get nodes
```

Note it is `kubectl`, not `k3s kubectl` — in this image `k3s` is a symlink to the
same multi-call binary, so `k3s kubectl` fails with a confusing
`unknown command "kubectl" for "kubectl"`.

## 2. Kubeconfigs on the host

Written somewhere scratch, so your `~/.kube/config` is left alone:

```bash
mkdir -p /tmp/xcs
docker exec xcs-alpha cat /etc/rancher/k3s/k3s.yaml \
  | sed 's|127.0.0.1:6443|127.0.0.1:16443|' > /tmp/xcs/kc-alpha.yaml
docker exec xcs-beta cat /etc/rancher/k3s/k3s.yaml \
  | sed 's|127.0.0.1:6443|127.0.0.1:16444|' > /tmp/xcs/kc-beta.yaml

export A="kubectl --kubeconfig=/tmp/xcs/kc-alpha.yaml"
export B="kubectl --kubeconfig=/tmp/xcs/kc-beta.yaml"
$A get ns && $B get ns
```

The published ports (`16443`, `16444`) are for *your* access from the host. The
controller uses `172.31.0.20:6443` — the in-network address. Do not mix them up:
from inside a Pod, `127.0.0.1` is that Pod.

---

## 3. A read-only identity in beta

```bash
$B apply -f - <<'EOF'
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
that at the *target* end means it holds regardless of what the controller does.

## 4. A workload in beta

```bash
$B create namespace demo
$B -n demo create deployment web --image=nginx:alpine --replicas=3
$B -n demo expose deployment web --port=80 --target-port=80 --type=NodePort --name=web
$B -n demo rollout status deploy/web

$B -n demo get svc web -o jsonpath='{.spec.ports[0].nodePort}{"\n"}'
$B -n demo get pods -o wide --no-headers | awk '{print $1, $6}'
```

Record both — you will check them against what the controller writes. In the
reference run: nodePort `31722`, Pod IPs `10.44.0.4`, `.5`, `.6`.

---

## 5. Build and deploy the controller into alpha

k3s uses containerd, so the image is imported directly rather than pulled:

```bash
docker build -t controller:latest .
docker save controller:latest | docker exec -i xcs-alpha ctr -n k8s.io images import -

export KUBECONFIG=/tmp/xcs/kc-alpha.yaml
make install
make deploy IMG=controller:latest
```

If `config/manager/manager.yaml` carries `imagePullPolicy: Always`, the kubelet
will ignore the imported image and fail to pull it from Docker Hub. Either drop
that line or patch the live object:

```bash
$A -n cross-cluster-service-system patch deploy cross-cluster-service-controller-manager \
  --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'

$A -n cross-cluster-service-system rollout status deploy/cross-cluster-service-controller-manager
$A -n cross-cluster-service-system logs -l control-plane=controller-manager --tail=30
```

Two things should be **absent** from those logs:

- any `POD_NAMESPACE` complaint — the Deployment sets it via the downward API,
  and the controller exits rather than guessing which namespace holds credentials
- any cluster-scoped Secret watch error — Secrets are cached from one namespace
  only ([cmd/main.go](../cmd/main.go)), and a cluster-wide watch here would mean
  that scoping regressed

Verify the second properly, against the live cluster rather than the manifests:

```bash
$A get clusterrole -o json | jq -r '.items[]
  | select(.metadata.name|startswith("cross-cluster-service"))
  | .metadata.name as $n | .rules[]
  | select(.resources[]? == "secrets") | "\($n): LEAK"'
# no output = no cluster-scoped secret grant (§9.1)

$A -n cross-cluster-service-system get role -o json | jq -r '.items[]
  | .metadata.name as $n | .rules[]
  | select(.resources[]? == "secrets") | "\($n): \(.verbs|join(","))"'
# cross-cluster-service-manager-role: get,list,watch
```

---

## 6. Register beta

```bash
$A -n cross-cluster-service-system create secret generic beta-creds \
  --from-literal=token="$($B -n kube-system get secret xcluster-reader-token \
      -o jsonpath='{.data.token}' | base64 -d)" \
  --from-literal=ca.crt="$($B -n kube-system get secret xcluster-reader-token \
      -o jsonpath='{.data.ca\.crt}' | base64 -d)"

$A apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: RemoteCluster
metadata:
  name: beta
spec:
  displayName: "k3s beta"
  access:
    type: Token
    token:
      server: https://172.31.0.20:6443
      secretRef: { name: beta-creds, key: token }
    tls:
      caSecretRef: { name: beta-creds, key: ca.crt }
  allowedNamespaces:
    matchNames: ["demo"]
EOF

$A get remotecluster beta -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

```
Authenticated=True (Authenticated)
Reachable=True (Reachable)
Ready=True (Ready)
```

The credential Secret must be in the controller's own namespace. `SecretKeyRef`
has no `namespace` field by design (§9.1): a cluster-scoped `RemoteCluster` able
to name any namespace's Secret would be a credential-exfiltration primitive.

No `tls.serverName` is needed here, because step 1 put the IP in the serving
certificate via `--tls-san`. Skip that flag and you will need
`serverName: kubernetes.default.svc` instead.

`allowedNamespaces` is not optional in practice: omitting it means **none**
(§9.2 fails closed), and every CrossService is refused with a condition saying
so. Step 11 tests that deliberately.

---

## 7. Wait — one gotcha about port names

Before writing a `Service` source, note how ports are matched. Port names are the
join key between your CrossService and the remote Service. `kubectl expose`
creates an **unnamed** port, so this fails:

```yaml
  ports:
    - name: http      # named locally...
      port: 80
```

```
SourcesResolved=False (PartialFailure: service demo/web has no port named "http")
```

The implicit single-port match at [service.go:110](../internal/resolver/service.go#L110)
only applies when the local port is *also* unnamed. With a named local port and an
unnamed remote one, bind explicitly with `remotePort`:

```yaml
  ports:
    - name: http
      port: 80
      remotePort: 80    # match the remote port by number
```

It fails loudly with an exact message rather than guessing, which is the intended
behaviour — but it is the first thing most people hit.

---

## 8. The datapath test: NodePort

This is the path that genuinely carries traffic between the two clusters, because
node IPs are Docker addresses on the shared network.

```bash
$A create namespace demo
$A apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata: { name: beta-web, namespace: demo }
spec:
  ports:
    - name: http
      port: 80
      remotePort: 80
  source:
    type: Service
    clusterRef: { name: beta }
    service:
      namespace: demo
      name: web
      via: NodePort
      nodePort: { addressType: InternalIP }
EOF

$A -n demo get endpointslice \
  -o custom-columns='NAME:.metadata.name,ADDRS:.endpoints[*].addresses,PORTS:.ports[*].port'
```

```
NAME              ADDRS            PORTS
beta-web-ipv4-0   [172.31.0.20]    31722
```

beta's **node IP** paired with the **allocated nodePort** — not Pod IPs, not port
80. The controller discovered `31722` by reading the remote Service; nothing
hardcoded it.

Now actually use it:

```bash
$A -n demo run probe1 --rm -i --restart=Never --image=curlimages/curl --command -- \
  curl -s -o /dev/null -w 'HTTP %{http_code}\n' --max-time 10 http://beta-web/
```

```
HTTP 200
```

The full chain: the controller authenticated to beta, read the Service, found the
nodePort, wrote an EndpointSlice, and kube-proxy in alpha programmed it.

---

## 9. The resolution test: Pod IPs

```bash
$A apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata: { name: beta-web-pods, namespace: demo }
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
```

```
NAME                   ADDRS                                 PORTS
beta-web-pods-ipv4-0   [10.44.0.4],[10.44.0.5],[10.44.0.6]   80
```

Beta's three Pod IPs, exactly. Scale beta's Deployment and the slice follows.

**Traffic will not flow yet**, and that is expected: the clusters run separate
flannel overlays, so alpha has no route to `10.44.0.0/16`. The EndpointSlice is
still correct — which is the part this controller is responsible for. On GKE this
same config *does* carry traffic, because VPC-native Pod IPs are real VPC
addresses. Two routes fix it locally:

```bash
docker exec xcs-alpha ip route add 10.44.0.0/16 via 172.31.0.20
docker exec xcs-beta  ip route add 10.42.0.0/16 via 172.31.0.10

$A -n demo run probe3 --rm -i --restart=Never --image=curlimages/curl --command -- \
  curl -s -o /dev/null -w 'HTTP %{http_code}\n' --max-time 10 http://beta-web-pods/
```

```
HTTP 200
```

Each node reaches the other's Pod subnet directly and flannel delivers locally
from there. The routes do not survive a container restart.

---

## 10. Address policy and DNS filtering

```bash
$A apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata: { name: static-guard, namespace: demo }
spec:
  ports: [{ port: 8080 }]
  source:
    type: Static
    static: { addresses: ["10.99.0.1", "169.254.169.254"] }
EOF

$A -n demo get events --field-selector involvedObject.name=static-guard \
  -o custom-columns='REASON:.reason,MSG:.message' --no-headers
```

```
AddressPolicyRejected   1 address(es) rejected by address policy, first: 169.254.169.254 (SpecialPurpose)
```

The metadata-server guard holds: `10.99.0.1` is written, `169.254.169.254` is
dropped with an event rather than silently.

DNS scope filtering, both directions:

```bash
$A apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata: { name: dns-public, namespace: demo }
spec:
  ports: [{ name: https, port: 443 }]
  source:
    type: DNS
    dns: { names: ["example.com."], recordType: A, excludePrivateIPs: true }
---
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata: { name: dns-private, namespace: demo }
spec:
  ports: [{ name: https, port: 443 }]
  source:
    type: DNS
    dns: { names: ["example.com."], recordType: A, excludePublicIPs: true }
EOF
```

`dns-public` keeps both answers and emits no event — nothing was dropped, so
nothing is reported. `dns-private` excludes both and says so:

```
NoEndpointsFound   dns: 2 of 2 address(es) for example.com. excluded as Public
```

Its conditions distinguish "resolved to nothing" from "failed to resolve", which
is the point:

```
SourcesResolved=True  (Resolved: source resolved)
Degraded=False        (NotDegraded)
Ready=False           (NoEndpointsFound: source resolved to no endpoints)
```

Setting both exclusions is refused at admission:

```bash
$A apply -f - <<'EOF'
# ...dns: { names: ["example.com."], excludePrivateIPs: true, excludePublicIPs: true }
EOF
```

```
The CrossService "dns-both" is invalid: spec.source.dns: Invalid value: "object":
excludePrivateIPs and excludePublicIPs cannot both be true; that would exclude every address
```

---

## 11. Namespace gating

```bash
$A create namespace other
$A apply -f - <<'EOF'
apiVersion: net.obaydullah.dev/v1alpha1
kind: CrossService
metadata: { name: sneaky, namespace: other }
spec:
  ports: [{ port: 80 }]
  source:
    type: Pods
    clusterRef: { name: beta }
    pods: { namespace: demo, selector: { matchLabels: { app: web } } }
EOF

$A -n other get crossservice sneaky \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}: {.message}){"\n"}{end}'
```

```
SourcesResolved=False (ClusterAccessDenied: namespace "other" is not permitted to reference RemoteCluster "beta")
Ready=False           (ClusterAccessDenied: namespace "other" is not permitted to reference RemoteCluster "beta")
```

Only `demo` was listed in `allowedNamespaces`. §9.2 fails closed.

---

## 12. Failure and recovery

```bash
docker stop xcs-beta
# wait, then:
$A get remotecluster beta -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
$A -n demo get endpointslice -l kubernetes.io/service-name=beta-web-pods \
  -o jsonpath='{range .items[*].endpoints[*]}{.addresses} ready={.conditions.ready}{"\n"}{end}'
```

```
Authenticated=True (Authenticated)
Reachable=False    (NetworkUnreachable: ...dial tcp 172.31.0.20:6443: connect: no route to host)
Ready=False        (NetworkUnreachable: ...)

["10.44.0.4"] ready=true
["10.44.0.5"] ready=true
["10.44.0.6"] ready=true
```

Endpoints are **retained**, not dropped — a transient apiserver blip must not
blackhole a working Service.

Note that the CrossService itself stays `Ready=True` while its RemoteCluster is
`Reachable=False`. Nothing re-reconciles it, so its status reflects the last
successful pass. Watch the RemoteCluster for source-cluster health; the
CrossService reports its own endpoints, not its cluster's reachability.

```bash
docker start xcs-beta
```

Recovery took roughly 80 seconds in the reference run, unattended.

---

## 13. Keeping a copy of the token

A legacy ServiceAccount token can be backed up and restored.

**Why it works.** The apiserver validates one by reading the `secret.name` claim
out of the JWT, looking that Secret up, comparing the stored bytes to the
presented token, and checking the ServiceAccount's UID against the
`service-account.uid` claim. Nothing is stateful beyond those objects. Restore
the Secret byte-for-byte and the token is live again.

**The one hard condition:** the ServiceAccount must still exist *with the same
UID*. Delete and recreate the SA and every token ever issued to it is dead — no
backup helps, because the UID baked into the JWT no longer matches.

Reconstruct the object rather than dumping it — a raw `get -o yaml` carries
`resourceVersion` and `uid`, which make the restore fail with a conflict:

```bash
cat > /tmp/xcs/token-backup.yaml <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: xcluster-reader-token
  namespace: kube-system
  annotations:
    kubernetes.io/service-account.name: xcluster-reader
type: kubernetes.io/service-account-token
data:
  token: $($B -n kube-system get secret xcluster-reader-token -o jsonpath='{.data.token}')
  ca.crt: $($B -n kube-system get secret xcluster-reader-token -o jsonpath='{.data.ca\.crt}')
  namespace: $($B -n kube-system get secret xcluster-reader-token -o jsonpath='{.data.namespace}')
EOF

# Without a matching SA UID the token is unrestorable, so record it too.
$B -n kube-system get sa xcluster-reader -o jsonpath='{.metadata.uid}' > /tmp/xcs/sa-uid
```

Restore, and check the UID still matches before trusting it:

```bash
$B apply -f /tmp/xcs/token-backup.yaml
[ "$($B -n kube-system get sa xcluster-reader -o jsonpath='{.metadata.uid}')" \
  = "$(cat /tmp/xcs/sa-uid)" ] && echo restorable || echo "SA recreated -- issue a new token"
```

**Handle it carefully.** That file is a live, non-expiring credential in
plaintext. Keep it out of `docs/` and out of git; encrypt it if it outlives the
afternoon (`age -p -o token.age token-backup.yaml`). Revoke by deleting the
Secret in beta — that invalidates every copy at once.

**Prefer regeneration.** Everything in step 3 is declarative, so losing the
Secret costs one re-apply plus one re-copy (step 6). The controller picks the new
credential up on its own — its client cache is keyed by credential fingerprint,
so rewriting the Secret rebuilds the connection with no restart. A backup you
never need beats a backup you have to protect.

---

## Appendix A: teardown

```bash
docker rm -f xcs-alpha xcs-beta
docker network rm xcs-net
rm -rf /tmp/xcs
```

---

## Appendix B: what to try next

- scale `web` in beta and watch the slice track it
- delete `beta-creds` and recreate it — the client cache is fingerprint-keyed, so
  the connection rebuilds with no restart
- add a second named port on both sides and confirm the join is by name
- point a `Service` source at `via: LoadBalancer` (k3s ships servicelb, so a
  LoadBalancer Service does get an address)

---

## Appendix C: what changes on GKE

| | this setup | GKE |
|---|---|---|
| Access type | `Token` | `Token` — unchanged |
| Pod IP routing | needs the two `ip route` commands | native (VPC-native alias IPs) |
| Pod CIDRs | `10.42/16`, `10.44/16` | `10.32/14`, `10.60/14`, `10.116/14` |
| `tls.serverName` | unneeded (`--tls-san`) | unneeded |
| Firewall | Docker network, open | **verify cross-cluster ingress rules** |
| Token expiry | never (legacy Secret) | same, with a legacy Secret |

The one that bites: the GKE Pod CIDRs sit below `10.128.0.0/9`, outside the
default VPC's `default-allow-internal` rule, and GKE's per-cluster rules only
admit each cluster's own Pod CIDR. Cross-cluster Pod traffic is likely blocked
until an ingress rule is added.

It fails as a **timeout, not an auth error** — and the CrossService will still
report Ready, because the controller verifies the remote *apiserver* is
reachable, not that the backends are. Nothing here health-checks endpoints.
