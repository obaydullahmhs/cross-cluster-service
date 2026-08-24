# Making Pod IPs routable between clusters, with Submariner

`via: PodIP` writes another cluster's Pod IPs into a local EndpointSlice. Those
addresses are only useful if packets can actually reach them, and by default they
cannot: most CNIs put Pod IPs in an overlay that ends at the cluster boundary.

This document covers establishing that connectivity. It is a prerequisite for
`via: PodIP` across clusters, not part of this controller.

> **Not yet executed end to end.** Unlike [testing-two-k3s.md](testing-two-k3s.md),
> where every command has been run and the output quoted, this document is written
> from Submariner's design and its documented interface. `subctl` flags move
> between releases — check the [upstream docs](https://submariner.io/operations/deployment/)
> for exact syntax, and treat `subctl diagnose all` as the authority on whether
> your setup is correct.

---

## Do you actually need this?

Pod-IP routing is the best data path when you can have it — one hop, real client
IPs, per-Pod readiness. It is also the most infrastructure. Check the cheaper
answers first:

| Situation | Do this instead |
|---|---|
| Clusters already share a VPC, non-overlapping Pod CIDRs | You may only need a **firewall rule**. Test before building anything. |
| Node IPs are mutually reachable | `via: NodePort` — no new components at all |
| Only a load balancer is reachable | `via: LoadBalancer` |
| Small, flat network, few nodes | Static routes: `ip route add <remote-pod-cidr> via <remote-node-ip>` |

The two-minute test that decides it, from a Pod in the consuming cluster:

```bash
kubectl run nettest --rm -it --restart=Never --image=nicolaka/netshoot -- sh -c '
  nc -zv -w3 <REMOTE_POD_IP> 80
  nc -zv -w3 <REMOTE_NODE_IP> 6443
'
```

- Pod IP answers, or refuses connection → routing works; you need nothing here
- Pod IP **hangs**, node IP answers → firewall between Pod CIDRs, or no route
- Both hang → no connectivity at all; Submariner or a VPN

Reach for Submariner when Pod IPs are genuinely unroutable, you cannot configure
nodes by hand, and you want the routing maintained as clusters change.

---

## How it works

Four components, only three of which matter for connectivity.

| Component | Runs where | Job |
|---|---|---|
| **Broker** | one designated cluster | A set of CRDs used as a rendezvous point: clusters publish their gateway address, CIDRs and public key, and read everyone else's. **No traffic passes through it.** |
| **Gateway Engine** | DaemonSet, only on nodes labelled `submariner.io/gateway=true` | Builds the inter-cluster tunnel. One active per cluster, active/passive HA by leader election. |
| **Route Agent** | DaemonSet, **every** node | Programs each node's routes so remote-CIDR traffic reaches the local gateway. This is what replaces configuring nodes by hand. |
| **Lighthouse** | optional | Cross-cluster DNS (`*.clusterset.local`). Not needed for connectivity — see [below](#if-you-are-already-using-crossservice). |

### The packet path

```
Pod in A                                              Pod in B
10.42.0.7                                            10.44.0.5
    │                                                     ▲
    │ 1. dst 10.44.0.5 — no local route                   │
    ▼                                                     │
node routing table   ← the route agent wrote this         │
    "10.44.0.0/16 via vx-submariner"                      │
    │                                                     │
    ▼  2. intra-cluster VXLAN (UDP 4800)                  │
Gateway node in A ─── IPsec or WireGuard ───► Gateway node in B
                      UDP 4500 / 500                 │
                   3. the only inter-cluster hop     ▼
                                            B's normal Pod network
```

Only the gateway node needs to reach the other cluster. Every other node forwards
to it over an intra-cluster VXLAN. That is why one labelled node per cluster is
enough, and why the gateway is the thing to make redundant.

---

## Prerequisites

### 1. Non-overlapping CIDRs — plan this first

The single most common setup failure. **Pod and Service ranges must not overlap**
across any pair of connected clusters.

```bash
# per cluster
kubectl cluster-info dump | grep -m1 -E 'cluster-cidr|service-cluster-ip-range'

# or, on a managed cluster where flags are hidden
kubectl get node -o jsonpath='{.items[0].spec.podCIDR}{"\n"}'
kubectl get svc kubernetes -n default -o jsonpath='{.spec.clusterIP}{"\n"}'
```

Write them down before installing anything:

| Cluster | Pod CIDR | Service CIDR |
|---|---|---|
| a | `10.42.0.0/16` | `10.43.0.0/16` |
| b | `10.44.0.0/16` | `10.45.0.0/16` |

If they overlap, you have two options, and the first is much better:

- **Renumber one cluster.** Painful, but it leaves you with a system where an IP
  means one thing. Usually only possible before production.
- **Globalnet.** Submariner assigns each exported Service a "global IP" from a
  separate range and does 1:1 NAT. It works, but you are no longer addressing real
  Pod IPs — verify what actually lands in your EndpointSlices, because
  `via: PodIP` will be writing global IPs.

### 2. Gateway reachability

The gateway nodes must be able to reach each other:

- **Same VPC / peered**: private addresses are fine, and preferable
- **Cross-cloud**: give each gateway node an external IP; IPsec traverses NAT over
  UDP 4500
- **Behind a site-to-site VPN**: use the private addresses the VPN carries

At least one side must be dialable. Two clusters both behind NAT with no inbound
path cannot form a tunnel.

### 3. Firewall

| Port | Between | Why |
|---|---|---|
| UDP `4500` | gateway ↔ gateway | IPsec NAT-T, or WireGuard |
| UDP `500` | gateway ↔ gateway | IKE — IPsec only, not needed for WireGuard |
| UDP `4800` | all nodes, within each cluster | intra-cluster VXLAN to the gateway |
| TCP `8080` | within each cluster | metrics |

Plus: the **broker cluster's API server** must be reachable from every joined
cluster.

---

## Install

```bash
curl -Ls https://get.submariner.io | bash
export PATH=$PATH:~/.local/bin
```

### 1. Deploy the broker

Any cluster can host it, including one of the two being connected. A separate
small cluster is tidier but not required.

```bash
subctl deploy-broker --kubeconfig ~/.kube/broker
```

This writes `broker-info.subm` to the working directory. **It contains
credentials** — treat it like a kubeconfig, and do not commit it.

### 2. Label a gateway node in each cluster

```bash
kubectl --kubeconfig ~/.kube/a label node <node-a> submariner.io/gateway=true
kubectl --kubeconfig ~/.kube/b label node <node-b> submariner.io/gateway=true
```

Label **two or three** per cluster in anything you care about. Failover is
leader-election based and takes seconds; a single labelled node is a single point
of failure for all cross-cluster traffic.

### 3. Join each cluster

```bash
subctl join broker-info.subm --kubeconfig ~/.kube/a \
  --clusterid a --cable-driver wireguard

subctl join broker-info.subm --kubeconfig ~/.kube/b \
  --clusterid b --cable-driver wireguard
```

**Prefer WireGuard** over the IPsec default unless you have a reason: one UDP
port, less to misconfigure, and far easier to debug.

Add `--globalnet` on both joins only if you established above that your CIDRs
overlap.

---

## Verify

```bash
subctl show all
subctl diagnose all
```

`subctl diagnose all` checks CIDR overlap, gateway reachability, firewall ports
and kube-proxy mode. **Run it before debugging anything else** — most "Submariner
is broken" reports are a closed UDP 4500 or overlapping CIDRs, and it names both
directly.

Then test the path this whole document exists for:

```bash
# a Pod IP in cluster b
POD_IP=$(kubectl --kubeconfig ~/.kube/b -n demo get pod -l app=api \
  -o jsonpath='{.items[0].status.podIP}')

kubectl --kubeconfig ~/.kube/a run nettest --rm -it --restart=Never \
  --image=nicolaka/netshoot -- curl -s -o /dev/null -w '%{http_code}\n' \
  --max-time 5 "http://$POD_IP:8080/"
```

A response means Pod IPs are routable and `via: PodIP` will carry traffic.

---

## Then use it

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
    clusterRef: { name: cluster-b }
    service:
      namespace: payments
      name: api
      via: PodIP
```

Callers use `payments.storefront.svc.cluster.local` — ordinary cluster DNS. One
hop to the remote Pod, real client IPs, and per-Pod readiness tracked from the
remote cluster's own Pod conditions.

Note the controller's credential path to the remote **apiserver** is separate from
this data path and unaffected by Submariner. See
[getting-started.md](getting-started.md) for the `RemoteCluster`.

---

## Troubleshooting

**`subctl diagnose all` first.** Almost everything below is something it reports.

**Connections hang rather than fail.** Firewall. A dropped packet times out; a
closed port refuses. Check UDP 4500 between gateway nodes, and UDP 4800 between
nodes within each cluster.

**Small requests work, large responses hang.** MTU. Encapsulation costs bytes and
you are past the path MTU. This is the classic Submariner symptom and it looks
like an application bug. Confirm it:

```bash
ping -M do -s 1400 <remote-pod-ip>     # then 1300, 1200
```

Fix by lowering the MTU on the CNI's interface, or enable TCP MSS clamping.

**Gateway shows `connecting` and never `connected`.** The gateways cannot reach
each other. Verify the addresses `subctl show endpoints` reports are actually
reachable — a node's *internal* IP is often published when the peer can only
reach its *external* one.

**Traffic works one way only.** Usually asymmetric firewall rules, or a return
route missing because one cluster's route agent has not learned the remote CIDR.
`subctl show connections` on both sides.

**Everything looks healthy, EndpointSlice is empty.** That is this controller, not
Submariner. Check the `CrossService` conditions and its events.

---

## If you are already using CrossService

Submariner ships **Lighthouse**, its own cross-cluster service discovery, which
resolves `<svc>.<ns>.svc.clusterset.local`. That overlaps with what a
`CrossService` does, so it is worth being deliberate.

Reasonable division:

- **Submariner for connectivity** — the routing, which is what this document is for
- **CrossService for endpoints and naming** — a Service in *your* namespace with
  *your* name, resolved by ordinary cluster DNS, so callers need no awareness that
  the backends are remote

Lighthouse asks applications to use `.clusterset.local`. If that is acceptable and
everything you consume is a Kubernetes Service in a joined cluster, Lighthouse
alone may be all you need, and it is one less component to operate. Try one
`ServiceExport` before deciding.

`CrossService` still covers what Lighthouse does not: `type: DNS` and `Static`
sources for things that are not Kubernetes at all, clusters outside the broker
set, and namespace-level access control that fails closed.

---

## Removing it

```bash
subctl uninstall --kubeconfig ~/.kube/a
subctl uninstall --kubeconfig ~/.kube/b
```

Any `CrossService` using `via: PodIP` will keep resolving correctly and stop
carrying traffic — the EndpointSlice stays accurate while the addresses become
unreachable. Switch those to `via: NodePort` or `via: LoadBalancer` **before**
uninstalling, or you will have a green CrossService serving dead addresses.
