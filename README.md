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
make test                  # operators-suite: all of the above, one at a time
make run W=cloudnative-pg  # a single workflow
```

## Demo script

1. **The platform.** Run `make status` and show `clusters/kind/operator-versions.yaml`: everything is from Git and pinned old.
2. **The app.** Run `make app`, then add a few todos. Point out the padlock (cert-manager), the meshed pods (`kubectl -n todo get pods` shows 2/2) and the Kyverno-labelled namespace.
3. **Green baseline.** Run `make test`. Walk through one workflow log (e.g. `cloudnative-pg`: failover time, zero lost writes).
4. **The upgrade.** Run `make upgrade`, which opens a PR that changes one file. Merge it, then run `flux reconcile ks flux-system --with-source`.
5. **Run the same tests again.** Run `make test`. Expected findings (all from real upstream changes):

| Workflow | What goes red | Why (upstream change) |
|---|---|---|
| `flux`, `cert-manager` | cert-manager stays on v1.18.6; the HelmRelease fails | v1.21 removed the Helm values `prometheus.servicemonitor.targetPort/path`. The chart schema now rejects our values, so the upgrade never happens. |
| `velero` | Velero server/backup fails | v1.17 removed the Restic uploader. Our `uploaderType: restic` is no longer valid. |
| `cloudnative-pg` | `cnpg_todo_items_open` disappears while the cluster stays "healthy" | v1.28.3/v1.29.1: the metrics exporter no longer runs as superuser (`pg_monitor` only), so custom queries on app tables fail silently. |
| `kyverno` | "policies written against a deprecated API" | v1.19 deprecates `ClusterPolicy` (removal planned in v1.20). Upgrading one more time would break admission. |
| `linkerd`, `external-secrets`, `todo-e2e` | stay green | These upgrades are safe for this app. That is useful information too. |

6. **Show the evidence.** Run `testkube get twe <execution-id>` and open the artifacts (metrics dumps, cert details, probe logs).
7. **Fix forward (optional).**
   - Drop the two prometheus values.
   - Set `uploaderType: kopia`.
   - `GRANT SELECT ON todos TO cnpg_metrics_exporter` (or move to `predicate_query`/a view).
   - Migrate the policies to `ValidatingPolicy`.

   Re-run the suite, and it goes green.

## Going to GKE later

Add a `clusters/gke/` entrypoint that reuses `infrastructure/` and `apps/` with these changes:
- a `ClusterSecretStore` for GCP Secret Manager instead of `secrets-vault`
- GCS instead of SeaweedFS for CNPG and Velero
- the app image pushed to a registry, not loaded with `kind load`
