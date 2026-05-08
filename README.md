# gpu-node-guardian

A Kubernetes controller that watches Node objects, scrapes the NVIDIA DCGM exporter for per-node GPU health, and cordons nodes whose GPUs are throwing errors or running too hot. Auto-uncordons on recovery. Emits Kubernetes Events so operators can audit decisions.

## What it does

```
DCGM exporter ── HTTP scrape ──> Scraper ── annotates Node ──> Reconcile ──> cordon / uncordon
```

Every 30 seconds the scraper:

1. Fetches Prometheus-format metrics from the DCGM exporter
2. Aggregates per-GPU metrics into a per-node summary (hottest GPU temperature, summed XID errors)
3. Writes the summary onto each Node as annotations

The reconcile loop watches Node objects. When the error-count annotation crosses a threshold (default 5) it cordons the node and emits a `NodeCordoned` Event. When the count recovers, it uncordons.

A self-applied annotation (`gpu-node-guardian/cordoned=true`) tracks which nodes were cordoned by this controller, so it never uncordons something a human cordoned for maintenance reasons.

## Why this design

**Separation of data source from decision logic.** The reconcile loop doesn't know or care where its data comes from. It reads an annotation. Today the annotation is set by a scraper hitting DCGM. Tomorrow it could be set by a webhook, a Prometheus alertmanager integration, or a node-side daemon. Decoupling these makes the controller easier to test and easier to extend.

**Per-node summary, not per-GPU.** The DCGM exporter emits one metric line per GPU per metric. A real GPU node has 8 GPUs. The controller does not need per-GPU detail to make a cordon decision; it needs to answer "is this node healthy enough to schedule on?" So the scraper collapses the data on the way through: hottest GPU temperature, summed XID errors. This is the right abstraction for the cordon layer.

**Idempotent reconcile.** Every Reconcile call must be safe to repeat. The decision is computed from current state (annotations + cordoned status), never from in-memory bookkeeping. Restarting the controller does not lose state.

**Patch over update.** Node objects are mutated by many actors (kubelet heartbeat, other controllers, manual operators). The controller only writes the fields it cares about, using `client.Patch` with a MergeFrom strategy so it never overwrites someone else's changes.

## Architecture

```
gpu-node-guardian/
├── cmd/main.go                              # entry point: builds manager, registers reconciler and scraper
├── internal/controller/
│   ├── gpunodehealth_controller.go          # Reconcile loop: cordon/uncordon based on annotations
│   └── scraper.go                           # periodic Runnable: scrape DCGM, write annotations
├── internal/dcgm/
│   └── client.go                            # HTTP client + Prometheus text-format parser
└── config/dcgm-mock/
    └── dcgm-mock.yaml                       # mock DCGM exporter for local testing
```

Three components, three responsibilities:

- **dcgm.Client**: knows how to talk to a DCGM exporter and return a per-node summary. No Kubernetes dependencies. Testable in isolation.
- **Scraper**: a controller-runtime `Runnable` that ticks every 30s, calls the dcgm client, and patches Node annotations. Implements graceful shutdown via context cancellation.
- **GPUNodeHealthReconciler**: the reconcile loop. Reads annotations. Cordons or uncordons. That's it.

## What's mocked vs production-ready

This repo runs end-to-end on a laptop with a mock DCGM exporter. The mock is honestly labeled and the integration patterns are real.

**Mocked:**

- The DCGM exporter is an Nginx pod serving a static text file in Prometheus format. Real DCGM requires NVIDIA hardware. The integration code is identical: hit `/metrics`, parse Prometheus text, summarize per node.
- "Inducing a fault" means editing a ConfigMap. In production it means a real GPU misbehaves and DCGM reports it.

**Real:**

- The Go code parses real Prometheus text format, including DCGM's specific metric names and labels.
- The cordon and uncordon mechanics are exactly what kubectl does under the hood (`spec.unschedulable`).
- The Runnable interface, manager registration, RBAC, Events, and patch semantics are all production patterns.

The substitution from mock to real DCGM is changing one config value (the URL) and deploying NVIDIA's GPU Operator instead of the mock manifest. No code changes.

## Running it locally

Prerequisites: Docker Desktop, [kind](https://kind.sigs.k8s.io/), kubectl, Go 1.22+, [kubebuilder](https://book.kubebuilder.io/).

```bash
# 1. Spin up a local Kubernetes cluster
kind create cluster --name gpu-toolkit --config=- <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
EOF

# 2. Deploy the mock DCGM exporter
kubectl apply -f config/dcgm-mock/dcgm-mock.yaml

# 3. Port-forward the exporter so the local controller can reach it
kubectl port-forward -n gpu-monitoring svc/dcgm-exporter 9400:9400 &

# 4. Run the controller (in a separate shell)
DCGM_EXPORTER_URL=http://localhost:9400/metrics make run
```

The controller starts scraping immediately. Within a few seconds, every Node will have annotations set by the scraper:

```bash
kubectl describe node gpu-toolkit-worker | grep -A 4 "Annotations"
```

## Demo: closed-loop cordoning

Inject a fault by editing the mock metrics:

```bash
kubectl edit configmap dcgm-mock-metrics -n gpu-monitoring
```

Find the line for `gpu-toolkit-worker` and change `XID_ERRORS ... 0` to `... 10`. Save and exit.

Force the mock pod to reload:

```bash
kubectl rollout restart deployment dcgm-mock -n gpu-monitoring
kubectl rollout status deployment dcgm-mock -n gpu-monitoring
```

Within 30 seconds (the next scrape tick), the controller logs show:

```
INFO  cordoned node due to GPU errors  {"node": "gpu-toolkit-worker", "errorCount": 10}
```

Verify:

```bash
kubectl get nodes
# gpu-toolkit-worker is now Ready,SchedulingDisabled

kubectl get events --field-selector reason=NodeCordoned
```

Reverting the value back to 0 triggers an automatic uncordon on the next scrape.

## Configuration

| Setting | Source | Default |
| --- | --- | --- |
| DCGM exporter URL | env `DCGM_EXPORTER_URL` | `http://dcgm-exporter.gpu-monitoring.svc.cluster.local:9400/metrics` |
| Scrape interval | constant in `scraper.go` | 30s |
| Cordon threshold (XID errors) | constant in `gpunodehealth_controller.go` | 5 |

In production these would move to a CRD or ConfigMap. See "Future work" below.

## Future work

In rough order of value:

1. **Move thresholds and intervals to a CRD or ConfigMap.** Hardcoded constants are fine for a demo, not for production.
2. **Real DCGM exporter integration.** Drop the nginx mock, deploy NVIDIA's GPU Operator. Code changes: zero.
3. **Distinguish recoverable vs unrecoverable XID errors.** XID 13 (graphics SM error) is often transient; XID 79 (GPU has fallen off the bus) requires a node drain. The controller should treat them differently.
4. **Slack or PagerDuty webhook on cordon.** Events alone are not enough for on-call; structured notifications belong in production.
5. **Prometheus metrics from the controller itself.** How many cordons today? Uncordon-to-cordon ratio? These are second-order signals operators want.
6. **Cordon-with-drain.** Right now cordoning leaves existing pods on the node. Production should optionally drain so customer workloads migrate before the GPU fully fails.

## What I learned building this

I came to this project as a senior infra engineer with deep Kubernetes operations experience but limited Go and no GPU-specific exposure. Building the controller end-to-end taught me three things I had not internalized before:

The kubebuilder operator pattern is mostly bookkeeping on top of a small idea. The "decide what to do" function is short. Most of the framework's value is in the watch-queue-retry plumbing it removes, and the RBAC scaffolding it generates.

DCGM metrics are dense and per-GPU, but the abstraction the cordon layer needs is per-node. Choosing where to do that aggregation (in the scraper, not the controller) was the design decision that made the rest fall into place.

Stragglers and noisy nodes are the silent killer in distributed GPU work, but they show up first as small statistical anomalies in metrics, not as hard failures. A reliability tool that only reacts to errors is too late.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) (or upstream kubebuilder boilerplate in source files).
