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
| Name, namespace, UID and resource version | Object identity and snapshot version |
| Labels | PDB selector matching |
| Owner references | ReplicaSet/Deployment discovery |
| `spec.nodeName` | Node index and displaced-Pod counting |

Container specifications, environment variables, volumes, annotations, managed
fields, Pod status and other unused metadata are not retained. The projection creates a new
Pod representation without mutating the incoming object. Non-Pod caches and all
existing Pod selectors are unchanged.

**This reduces retained bytes per Pod, not the number of cached Pods.**
Watches still receive full Pod objects, and
decoding, initial population, queues and other transient allocations still cost
memory. There is no claim that every cluster now fits within 128Mi or 512Mi.

## Why the controller no longer writes Pod status

The node controller records an eviction signal on the matching EAS's
`Spec.LastEviction`; that is what the surge controller consumes. Writing the
anticipatory `DisruptionTarget` Pod condition was not required for this path.

This change intentionally stops publishing that condition. The node controller
performs no Pod GET, UPDATE or status PATCH after its filtered Pod list. It does
not delete or evict Pods. This removes a Pod API write/failure point and permits
dropping the entire Pod status from the compact cache. External consumers of the
old anticipatory condition will no longer receive it from EAS.

A projected Pod is a **read model**, not a complete object suitable for Update.
Do not add Pod writes from these incomplete objects without revisiting the design.

Existing Pod-status permissions are retained only for older controller images
during rolling upgrades and rollbacks; the new controller does not use them.
The [projection/reconciliation tests](../internal/controller/pod_cache_test.go)
reject per-Pod reads/writes and verify that LastEviction is still recorded while
the server-side Pod remains unchanged.

## Node reconciliation lookup reuse

The [node reconciler](../internal/controller/node_reconciler.go) lists Pods once and
processes them in one traversal. On the first Pod in a namespace, it loads that
namespace's EAS resources into an owned local snapshot. Later Pods reuse that
snapshot, including an empty result. This avoids repeated cache scans and copies;
the namespace map is not a second persistent cache and does not deduplicate writes.

Matching preserves the first applicable EAS for each Pod. LastEviction is still
updated for every matched Pod; the returned resource version is retained for later
updates to the same EAS within this pass. A list failure in a later namespace can
occur after earlier Pods have been processed; errors return for normal retry.
PDB Gets are still cache-backed and are not separately deduplicated.

This lookup optimization is independent of the retained Pod-memory mitigation.
Once-per-EAS update deduplication is a separate follow-up.

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
| API server and behavior | Existing controller/envtest and e2e suites | CRD installation, read-only Pod handling and eviction signals |

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
