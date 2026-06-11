# Running on EKS

A complete, reproduced-for-real walkthrough of the pgbranch stack on AWS —
branchd, the webhook service, the production Postgres, and every branch pod
in one small EKS cluster, with GitHub and Vercel talking to it over public
LoadBalancers. Everything below was executed, not imagined; the bugs at the
end were found doing it.

## Why in-cluster is the natural deployment

Running pgbranch on a laptop against cloud consumers needs a public TCP
tunnel for the proxy, a webhook forwarder, and (for managed-Postgres
sources) dump-based seeding. In-cluster, all of that disappears:

| concern | laptop / no-infra | in-cluster |
|---|---|---|
| webhook delivery | smee/tunnel forwarder | ghook behind a LoadBalancer, GitHub posts directly |
| proxy reachability | tunnel (expiring, random address) | stable LoadBalancer DNS |
| seeding | `--via dump` (managed clouds block basebackup) | `pg_basebackup` from the in-cluster replica/primary |
| endpoints in CI/Vercel | re-wired on every tunnel restart | set once |

## Provision

`deploy/terraform/eks` holds a minimal single-node cluster (default VPC,
one `t3.large`, managed node group):

```bash
cd deploy/terraform/eks
terraform init && terraform apply
aws eks update-kubeconfig --name pgbranch --region ap-south-1
```

**Cost while running:** control plane ~$0.10/h, node ~$0.09/h, plus ~$0.03/h
per LoadBalancer Service (two here). Mind the Kubernetes version: clusters
on versions past their standard-support window are billed AWS *extended
support* (~6× the control-plane rate). Check what's current:

```bash
aws eks describe-cluster-versions \
  --query 'clusterVersions[].{v:clusterVersion,status:versionStatus}'
```

## Images

The Helm chart defaults to locally-built `pgbranch/branchd:dev` images. For
a real cluster, push to a registry — and note two practical traps:

- **Cross-compile on the host** (`GOOS=linux GOARCH=amd64 CGO_ENABLED=0`,
  pure-Go thanks to modernc.org/sqlite) and build a copy-only image.
  Running the Go toolchain under qemu emulation on Apple Silicon segfaults.
- **GHCR packages default to private.** Either make them public or create a
  pull secret and attach it to the service accounts (the chart's own SA for
  branchd, and `default` for branch/helper pods):

```bash
kubectl -n pgbranch create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io --docker-username=<user> --docker-password=<token>
kubectl -n pgbranch patch serviceaccount pgbranch \
  -p '{"imagePullSecrets":[{"name":"ghcr-pull"}]}'
kubectl -n pgbranch patch serviceaccount default \
  -p '{"imagePullSecrets":[{"name":"ghcr-pull"}]}'
```

## Deploy

```bash
helm install pgbranch deploy/helm/pgbranch -n pgbranch --create-namespace \
  --set node=<storage-node-name> \
  --set image.repository=ghcr.io/<user>/pgbranch-branchd --set image.tag=<tag> \
  --set token=$(openssl rand -hex 16) \
  --set proxy.service.type=LoadBalancer \
  --set ghook.enabled=true \
  --set ghook.image.repository=ghcr.io/<user>/pgbranch-ghook --set ghook.image.tag=<tag> \
  --set ghook.webhookSecret=$(openssl rand -hex 16) \
  --set ghook.githubToken=<token-with-issues-write> \
  --set ghook.source=prod --set ghook.resetOnPush=true \
  --set ghook.repos=<owner>/<repo> \
  --set ghook.service.type=LoadBalancer
```

`type: LoadBalancer` on EKS provisions Classic ELBs out of the box (raw TCP
— exactly what the wire-protocol proxy needs; no aws-load-balancer-controller
required). Once the proxy ELB has a hostname, feed it back so PR comments
show the right address:

```bash
helm upgrade pgbranch deploy/helm/pgbranch -n pgbranch --reuse-values \
  --set ghook.proxyHost=$(kubectl -n pgbranch get svc pgbranch-proxy \
    -o jsonpath='{.status.loadBalancer.ingress[0].hostname}'):6432
```

Point the GitHub webhook at
`http://<ghook-elb>:8080/webhook` (`pull_request` events, the same secret) —
deliveries are HMAC-verified, so public exposure is by design. Seed the
source the native way (`pgb source add` against the in-cluster service via a
port-forward of `pgbranch-api`), and external consumers (CI, Vercel) connect
through the proxy ELB with `dbname@branch`.

## Upgrading Kubernetes

EKS moves one minor version at a time. `cluster_version` is a Terraform
variable for exactly this:

```bash
for v in 1.33 1.34 1.35 1.36; do
  terraform apply -auto-approve -var cluster_version=$v
done
```

Each step upgrades the control plane (~10 min) and rolls the node group.
pgbranch itself is indifferent — it uses only stable v1 APIs — but
**hostpath mode keeps all CoW data and the registry on the storage node's
disk, and a node rollover recycles that disk**. Branches are disposable by
design, so the procedure is: upgrade, then re-seed sources and let the
webhook recreate PR branches (or `pgb branch create` what you need). If
branch survival across node loss matters, use `storage.mode=csi` — PVC
clones live in EBS, not on the node.

## Teardown

```bash
kubectl -n pgbranch delete svc pgbranch-proxy pgbranch-ghook   # release the ELBs
terraform -chdir=deploy/terraform/eks destroy
```

## What deploying here taught us (three real bugs)

All three were invisible on laptop Docker and surfaced within an hour of
running on EKS — they are why "works in kind" is not "works in production":

1. **Branches recorded an empty address** (`fix(engine)` in `c15874b`).
   Kubernetes pods answer exec probes seconds before the kubelet's status
   sync publishes `status.podIP`. The engine inspected once right after
   readiness, stored `host:""`, and the proxy dialed `:5432`. It now polls
   until the runtime reports a routable address. The kind integration tests
   missed it because they verify connectivity via port-forward rather than
   the registry's recorded endpoint.

2. **GitHub webhook deliveries cancelled branch operations mid-saga**
   (`fix(ghook)` in `c15874b`). GitHub abandons deliveries after ~10s; the
   handler ran branch operations on the request context, so the disconnect
   cancelled branchd's saga mid-flight. Docker resets finished in ~7s and
   never hit it; pod resets take ~12s and hit it every time. The saga
   compensations unwound correctly (the branch ended `failed`, no orphans —
   the design held), but the operation was lost. ghook now acks `202`
   immediately and runs operations on a detached five-minute context,
   draining in-flight work on shutdown.

3. **CI raced async branch creation.** With the instant ack, a fast runner
   reaches `psql` before the branch pod is ready. Consumers should wait for
   connectivity — see the retry loop in the
   [demo repo's workflow](https://github.com/abd-ulbasit/pgbranch-demo/blob/main/.github/workflows/pr-db-check.yml).
