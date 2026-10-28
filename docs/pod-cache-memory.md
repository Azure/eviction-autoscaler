# Pod cache memory: OOM cause and mitigation

## What this fixes

The original eviction-autoscaler retained full Pod objects for the entire cluster
in a controller-runtime informer cache. On affected Cosmic test clusters, this
exceeded the chart's 128Mi limit and caused repeated OOMKills and crashloops.
Dogfood and production have larger clusters, so test-ring behavior is not a safe
upper bound. [Issue #203](https://github.com/Azure/eviction-autoscaler/issues/203)
contains the reported impact and rollout checklist.

**The fix is to discard unused fields before storing Pods in the informer cache.**
It is not a TTL, a namespace enablement filter, or merely removal of an index.

## Why existing List filters did not bound the cache

The original node-name `IndexField(Pod)` registration requests a Pod informer.
Default cached Pod Get/List calls can also create that informer on demand.
The informer retains all Pods in its configured collection scope, which is
cluster-wide here.

`List` options such as namespace, PDB label selector, or `spec.nodeName` filter the
results returned to a caller. They do not narrow the Pod population retained by
the shared informer. Similarly, `ACTIONED_NAMESPACES`, namespace annotations and
`enabledByDefault` control which workloads EAS manages, not what the cache stores.

```text
Before:
  cluster-wide full Pod list/watch
      -> store full Pods + node-name index
      -> filter cached objects for each reconciliation

After (default):
  cluster-wide full Pod list/watch
      -> TransformPodCache
      -> store compact Pods + node-name index
      -> filter cached objects for each reconciliation
```

The change is in [TransformPodCache](../internal/controller/pod_cache.go), wired
through `cache.Options.ByObject` in [manager setup](../cmd/main.go).

## What is retained

| Retain | Reason |
| --- | --- |
| Name, namespace, UID and resource version | Identity and optimistic concurrency |
| Labels | PDB selector matching |
| Owner references | ReplicaSet/Deployment discovery |
| `spec.nodeName` | Node index and displaced-Pod counting |
| Complete `Pod.Status` | Preserve existing status and condition-reporting behavior |

Container specifications, environment variables, volumes, annotations, managed
fields and other unused metadata are not retained. The projection creates a new
Pod representation without mutating the incoming object. Non-Pod caches and all
existing Pod selectors are unchanged.

**This reduces retained bytes per Pod, not the number of cached Pods.** Full
status can still be substantial. Watches still receive full Pod objects, and
decoding, initial population, queues and other transient allocations still cost
memory. There is no claim that every cluster now fits within 128Mi or 512Mi.

## Why Pod status writes use patches

The node controller still reports the anticipatory `DisruptionTarget` condition.
EAS's `LastEviction` remains the surge signal; this change does not remove the
existing observable Pod condition.

A projected Pod is a **read model**, not a complete object suitable for Update.
Even `Status().Update` can clear omitted annotations. Keeping status alone does
not make a full status update safe when metadata has been discarded.

[markPodDisruptionTarget](../internal/controller/node_reconciler.go) instead:

1. Deep-copies the cached snapshot before changing the condition.
2. Computes a merge status patch against that snapshot.
3. Includes the original resource version as an optimistic-lock precondition.
4. Returns conflicts for normal controller retry rather than overwriting newer
   state.

The patch preserves server-side annotations/spec, other conditions, container
statuses and Pod addresses. RBAC adds Pod-status `patch` permission while retaining
`update` for older controller instances during rolling upgrades and rollbacks.
The [cache integration test](../internal/controller/node_cache_test.go) exercises
this against an API server and verifies preservation and stale-version conflicts.

## Node reconciliation lookup reuse

The [node reconciler](../internal/controller/node_reconciler.go) lists EAS resources
once per distinct Pod namespace, before its main Pod loop. The snapshots are local
to that reconciliation, not a second persistent cache.

Matching returns an owned EAS object from that snapshot. Successful updates retain
the returned resource version so subsequent Pods matching the same EAS do not use
the initial stale version. First-match semantics and error propagation remain;
PDB Gets are still cache-backed and are not separately deduplicated.

The node controller uses one worker by default. Other controllers have independent
workers, so this is not a global concurrency or API request-rate limit.

## Configuration and rollout

```yaml
controllerConfig:
  podCache:
    enabled: true
resources:
  requests:
    memory: 128Mi
  limits:
    memory: 512Mi
```

- **Default true:** compact watch-backed Pod cache and node-name index.
- **Explicit false:** direct API Pod Get/List fallback, with the same selectors.
  The manager bypasses Pod caching and the node controller does not register its
  Pod index. Other resource caches remain enabled.
- Existing explicit false overrides remain false on upgrade. Set the value to
  true to select compact caching.
- Memory and environment changes update the Pod template and roll the controller
  Pod. This is not hot reload, automatic memory sizing, or in-place Pod resizing.
  Startup and leader election may briefly pause processing.
- The Cosmic patch handler is outside this repository. Its deployment pipeline
  must apply the intended image, chart values and RBAC together.

Before higher-ring promotion, measure retained heap, cold-start and concurrent-drain
peaks on representative large clusters, and monitor OOMs, restarts, reconciliation
latency and API-server traffic. Test the direct-read fallback separately if used.

## Kubernetes-aware CI validation

CI checks complementary layers; no single tool establishes memory capacity:

| Layer | Command | What it checks |
| --- | --- | --- |
| Helm | `make helm-validate` | Chart lint/rendering and configuration regression tests |
| Memory semantics | Go preflight within `make helm-validate` | Kubernetes-native quantity parsing, non-negative memory, request <= limit |
| Kubernetes schemas | `make helm-schema-validate` | Strict Kubeconform validation of rendered built-in resources |
| API server and behavior | Existing controller/envtest and e2e suites | CRD installation, API behavior and reconciliation/status preservation |

Kubeconform is pinned to **v0.8.0**. Schema validation covers both compact-cache and
direct-read configurations against **1.30.2** (the e2e Kind configuration) and the
Makefile's **ENVTEST_K8S_VERSION**, currently **1.32.0**. Unknown fields, invalid
resource shapes and missing schemas fail validation. Schemas are downloaded from
Kubeconform's upstream Kubernetes schema registry and cached under `bin/`; registry
access is required on a cold cache.

The tool's source build requires Go 1.26; the existing tool installer uses automatic
Go toolchain selection. This is a build-tool requirement, not a change to the
controller's Go module version. A verified prebuilt executable can instead be
selected with the `KUBECONFORM` Makefile override.

The sole explicit schema skip is
`apiextensions.k8s.io/v1/CustomResourceDefinition`: the upstream registry does not
provide that definition's schema for these versions. This is not a blanket
`-ignore-missing-schemas`. CRD installation remains covered by the existing
API-server/envtest and chart e2e tests. This chart does not render custom EAS
resource instances.

Run locally:

```bash
make helm-validate
make helm-schema-validate
```

To validate custom memory overrides before deployment:

```bash
helm template eviction-autoscaler ./helm/eviction-autoscaler \
  --namespace kube-system --show-only templates/deployment.yaml \
  --set resources.requests.memory=128Mi \
  --set resources.limits.memory=512Mi \
  | go run ./cmd/validate-chart-resources
```

Standalone Helm rendering and JSON-schema checks do not compare memory requests
against limits. Run the preflight as well; Kubernetes admission remains the final
guard if a deployment path bypasses the repository's CI/preflight.
