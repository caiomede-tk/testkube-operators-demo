# Testkube operators demo. Run `make help` for targets.
CLUSTER      ?= operators-demo
GITHUB_OWNER ?= caiomede-tk
GITHUB_REPO  ?= testkube-operators-demo
APP_IMAGE    := todo:v1.0.0

export KUBECONFIG := $(CURDIR)/.kind/kubeconfig
# Demo-local Testkube CLI config (.kind/.testkube), so your own CLI contexts stay untouched.
TK := HOME=$(CURDIR)/.kind testkube

.PHONY: help kind-up app-image bootstrap status test run app upgrade kind-down

help: ## List targets
	@grep -hE '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

kind-up: ## Create the kind cluster (kubeconfig in .kind/, your global config is untouched)
	@mkdir -p .kind
	kind get clusters | grep -qx $(CLUSTER) || kind create cluster --config kind.yaml --name $(CLUSTER)
	kind get kubeconfig --name $(CLUSTER) > $(KUBECONFIG)
	$(TK) set context --kubeconfig --namespace testkube >/dev/null

app-image: ## Build the ToDo image and load it into kind
	docker build -t $(APP_IMAGE) app
	kind load docker-image $(APP_IMAGE) --name $(CLUSTER)

bootstrap: ## Install Flux and point it at this repo (needs GITHUB_TOKEN)
	flux bootstrap github --owner=$(GITHUB_OWNER) --repository=$(GITHUB_REPO) --personal \
	  --branch=main --path=clusters/kind

status: ## Show what Flux has reconciled
	flux get kustomizations
	kubectl get helmreleases -A

SUITE := flux cert-manager linkerd kyverno external-secrets cloudnative-pg velero todo-e2e

# One at a time: the workflows deliberately disturb the shared environment (failover, rotation, restore).
# (Testkube OSS standalone has no `execute` step, so the suite is driven from here.)
test: ## Run every workflow in order and summarise (full logs in .kind/logs/)
	@mkdir -p .kind/logs; rc=0; for w in $(SUITE); do \
	  printf '%-18s ' $$w; \
	  if $(TK) run testworkflow $$w -f > .kind/logs/$$w.log 2>&1; then echo PASSED; else echo FAILED; rc=1; \
	    perl -pe 's/\e\[[0-9;]*m//g' .kind/logs/$$w.log | grep -a 'FINDING' | sed 's/^/    /'; fi; \
	done; exit $$rc

run: ## Run one workflow: make run W=cloudnative-pg
	$(TK) run testworkflow $(W) -f

app: ## Open the ToDo app on https://localhost:8443
	kubectl -n todo port-forward svc/todo 8443:443

upgrade: ## Open the "upgrade operators" PR; Flux rolls it out once merged
	git switch -c upgrade-operators
	cp upgrade/operator-versions.yaml clusters/kind/operator-versions.yaml
	git commit -am "Upgrade operators to their latest releases"
	git push -u origin upgrade-operators
	gh pr create --fill

kind-down: ## Delete the kind cluster
	kind delete cluster --name $(CLUSTER)
