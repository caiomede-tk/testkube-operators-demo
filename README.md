# Testkube operators demo

Six Kubernetes operators run pinned 2–3 releases behind the latest, all installed by Flux.
A ToDo app uses every one of them, and Testkube TestWorkflows test each operator's actual behaviour, beyond its pods being Running.
The demo ends with a one-file Git commit that upgrades all operators. The same workflows then catch what the upgrade broke.

| Operator | Pinned (old) | Upgrade target | Role in the app |
|---|---|---|---|
| Flux | v2.10 (bootstrap) | – | Installs everything from this repo; an upgrade is a Git commit |
| cert-manager | v1.18.6 | v1.21.3 | Demo CA → app HTTPS cert; Linkerd trust anchor + identity issuer |
| Linkerd (edge) | edge-26.7.2 | edge-26.10.1 | mTLS everywhere; only the app's identity may reach PostgreSQL |
| Kyverno | v1.16.4 | v1.19.2 | Guardrails on app namespaces (pinned tags, non-root, limits, mesh, seccomp, quota) |
| External Secrets | v2.11.0 | v2.14.0 | DB and S3 credentials from the "vault" (GCP Secret Manager on GKE) |
| Velero | v1.16.2 | v1.18.2 | Namespace disaster recovery (drill into `todo-restore`) |
| CloudNativePG | v1.27.1 | v1.30.1 | 3-instance PostgreSQL with synchronous replication, S3 backups, business metrics |

Versions live in **one file**: [clusters/kind/operator-versions.yaml](clusters/kind/operator-versions.yaml).
The upgrade target is [upgrade/operator-versions.yaml](upgrade/operator-versions.yaml).
Linkerd note: open-source *stable* releases stopped at 2.14, so open-source users run the **edge** channel. That is a good talking point about why operator upgrades need testing.

## Layout

```
clusters/kind/        Flux entrypoint: operator-versions + ordered Kustomizations (stack.yaml)
infrastructure/
  sources/            Helm repositories
  operators/          cert-manager, Kyverno, External Secrets, CNPG, Linkerd CRDs
  configs/            PKI, Kyverno policies, "vault" + ClusterSecretStore, S3 (SeaweedFS)
  platform/           Linkerd control plane, Velero
apps/todo/            namespace, ExternalSecrets, CNPG Cluster, Linkerd policy, app, Velero schedule
app/                  the ToDo monolith (Go, one file)
testkube/agent/       Testkube OSS agent
testkube/workflows/   one TestWorkflow per operator + todo-e2e + operators-suite
```

Install order (Flux `dependsOn`): `sources → operators → configs → platform → apps`, with `testkube → tests` in parallel.

## Setup (kind)

You need docker (12 GB of RAM recommended), kind, kubectl, flux, the testkube CLI, gh, and a GitHub token.

```sh
make kind-up app-image                      # cluster + ToDo image loaded into kind
GITHUB_TOKEN=$(gh auth token) make bootstrap
make status                                 # wait until every Kustomization is Ready (~15 min on first run)
make app                                    # https://localhost:8443 (demo CA, so expect a browser warning)
```

Every command uses `.kind/kubeconfig`, so your global kubeconfig is untouched.

## What each TestWorkflow proves

| Workflow | Beyond the green lights |
|---|---|
| `flux` | Every HelmRelease is Ready at the exact version in Git. Manual drift is reverted. `suspend` is honoured and `resume` restores the Git state. |
| `cert-manager` | The cert chains to the CA with the right SANs and is what the app actually serves. A forced renewal produces a new key, which the app serves without a restart. After the Linkerd issuer is rotated, the mesh still carries app→DB traffic. |
| `linkerd` | All app and DB pods are meshed. The DB proxy sees mTLS from the app's identity. A wrong ServiceAccount and an unmeshed client are denied, while the app's SA is allowed. |
| `kyverno` | Each Enforce policy blocks its violation. A compliant pod is mutated. The quota is generated. Policies use no deprecated API. |
| `external-secrets` | A vault rotation reaches the Secret, the PostgreSQL role (old password rejected) and the app's new connections. A broken vault entry is reported while the last good Secret is kept. |
| `cloudnative-pg` | The business metric (custom query) exists and matches the app. The primary is killed under write load with zero acknowledged writes lost and a bounded failover. A backup restores into a new cluster. |
| `velero` | Full DR drill: namespace backup → restore into `todo-restore`, with the DB re-bootstrapped from CNPG backups through resource modifiers. Data, mesh and policies all come back. |
| `todo-e2e` | Playwright through the HTTPS UI: add, complete, delete. |

```sh
make test                  # all of the above, one at a time, with a PASSED/FAILED summary
make run W=cloudnative-pg  # a single workflow
```

### What the workflows caught while this demo was being built

These are good stories for the talk, because each one happened while every operator reported healthy:

- **The business metric was never exported.** The CNPG exporter queries as `pg_monitor`, which could not read the app's table. The Cluster stayed "healthy" the whole time. Fixed with `ALTER DEFAULT PRIVILEGES … GRANT SELECT … TO pg_monitor` in `postInitApplicationSQL`.
- **The Velero restore "succeeded" but the database never came back.** Velero restored CNPG's generated Services and Secrets, and CNPG refuses to adopt objects it does not own. Fixed by labelling everything CNPG generates `velero.io/exclude-from-backup` (`inheritedMetadata`). Velero now carries the declarative state, and CNPG backups carry the data.

## Demo script

1. **The platform.** Run `make status` and show `clusters/kind/operator-versions.yaml`: everything is from Git and pinned old.
2. **The app.** Run `make app`, then add a few todos. Point out the padlock (cert-manager), the meshed pods (`kubectl -n todo get pods` shows 2/2) and the Kyverno-labelled namespace.
3. **Green baseline.** Run `make test`. Walk through one workflow log (e.g. `cloudnative-pg`: failover time, zero lost writes).
4. **The upgrade.** Run `make upgrade`, which opens a PR that changes one file. Merge it, then run `flux reconcile source git flux-system`.
   Within about 5 minutes, CNPG, External Secrets, Kyverno and the Linkerd CRDs are upgraded. cert-manager is rejected.
5. **Act 1: run the same tests again** with `make test`. These results were verified on kind:

| Workflow | Result | Why (real upstream change) |
|---|---|---|
| `flux` | ❌ cert-manager, linkerd-control-plane and velero are not at the Git version | One rejected upgrade blocks the dependent layers (`operators` → `configs` → `platform`), so Linkerd's control plane and Velero are never even attempted. |
| `cert-manager` | ❌ still v1.18.6 | v1.21 removed the Helm values `prometheus.servicemonitor.targetPort/path`. The chart schema rejects our values. |
| `linkerd` | ❌ control plane still on edge-26.7.2 | Blocked behind cert-manager (see `flux`). |
| `kyverno` | ❌ "ClusterPolicy is deprecated … migrate to ValidatingPolicy" | v1.19 deprecates `kyverno.io/v1` policies (removal planned for v1.20). Everything still works today; the next upgrade will break admission. |
| `cloudnative-pg` | ❌ `cnpg_todo_items_open` is gone, while the cluster still reports healthy | v1.28.3/v1.29.1: the exporter now connects as `cnpg_metrics_exporter` instead of `postgres`. Our hardening (`REVOKE CONNECT … FROM PUBLIC`) shuts it out with "permission denied for database". |
| `velero` | ❌ still v1.16 | Blocked behind cert-manager. |
| `external-secrets`, `todo-e2e` | ✅ | These upgrades are safe for this app. That is useful information too. |

6. **Act 2: fix forward, find the next one.** Delete the two `prometheus.servicemonitor` lines in [infrastructure/operators/operators.yaml](infrastructure/operators/operators.yaml), commit, and push.
   cert-manager v1.21 and Linkerd edge-26.10.1 now roll out, and `cert-manager` and `linkerd` go green.
   Velero is next, and its chart refuses the upgrade with `ERROR: restic uploaderType was removed` (v1.17 removed Restic). `velero` and `flux` stay red.
7. **Show the evidence.** Each workflow saves artifacts: metrics dumps, cert details, probe logs, admitted objects.
   Use `HOME=.kind testkube get twe` to list the executions, and `HOME=.kind testkube download artifacts <id>` to fetch them.
   Full logs are in `.kind/logs/`.
8. **Remaining fixes (optional homework).**
   - Set `uploaderType: kopia` in [infrastructure/platform/velero.yaml](infrastructure/platform/velero.yaml).
   - Run `GRANT CONNECT ON DATABASE todo TO cnpg_metrics_exporter` (and add it to `postInitApplicationSQL`).
   - Migrate the Kyverno policies to `ValidatingPolicy`/`MutatingPolicy`/`GeneratingPolicy`.

**Resetting the demo** after a run: revert the upgrade and fix commits on `main`, then run `make kind-down kind-up app-image bootstrap`. Operators are not downgraded in place.

## Going to GKE later

Add a `clusters/gke/` entrypoint that reuses `infrastructure/` and `apps/` with these changes:
- a `ClusterSecretStore` for GCP Secret Manager instead of `secrets-vault`
- GCS instead of SeaweedFS for CNPG and Velero
- the app image pushed to a registry, not loaded with `kind load`
