# Running on EKS

A complete, reproduced-for-real walkthrough of the pgoverlay stack on AWS —
branchd, the webhook service, the production Postgres, and every branch pod
in one small EKS cluster, with GitHub and Vercel talking to it over public
LoadBalancers. Everything below was executed, not imagined; the bugs at the
end were found doing it.

## Why in-cluster is the natural deployment

Running pgoverlay on a laptop against cloud consumers needs a public TCP
tunnel for the proxy, a webhook forwarder, and (for managed-Postgres
sources) dump-based seeding. In-cluster, all of that disappears:

| concern | laptop / no-infra | in-cluster |
|---|---|---|
| webhook delivery | smee/tunnel forwarder | ghook behind a LoadBalancer, GitHub posts directly |
| proxy reachability | tunnel (expiring, random address) | stable LoadBalancer DNS |
| seeding | `--via dump` (managed clouds block basebackup) | `pg_basebackup` from the in-cluster standby (recommended) or primary |
| endpoints in CI/Vercel | re-wired on every tunnel restart | set once |

## Provision

`deploy/terraform/eks` holds a minimal single-node cluster (default VPC,
one `t3.large`, managed node group). Two inputs have no safe default and
are asked for: who gets the budget alerts, and, optionally, which addresses
may reach the Kubernetes API. Without
`cluster_endpoint_public_access_cidrs` the API endpoint is private, so
`kubectl` works only from inside the VPC; give your own egress address as a
`/32` to run it from a laptop (`0.0.0.0/0` is refused):

```bash
cd deploy/terraform/eks
terraform init
terraform plan -out tfplan \
  -var 'budget_alert_emails=["you@example.com"]' \
  -var "cluster_endpoint_public_access_cidrs=[\"$(curl -fsS https://checkip.amazonaws.com)/32\"]"
terraform apply tfplan
aws eks update-kubeconfig --name pgoverlay --region ap-south-1
```

The provider and Terraform versions are pinned (`.terraform.lock.hcl` is
committed). State is local: this is a single-operator demo, and the
`backend` comment in `main.tf` shows the S3 set-up (with `use_lockfile`) to
use for anything shared. `make tf-test` checks the configuration offline.

**What is not in Terraform.** Only the cluster and the budget are. The Helm
release, the GHCR pull secret, cert-manager and the Secrets below are applied
by hand from this page, and the load balancers and their security groups are
created by Kubernetes for the `LoadBalancer` Services; none of it is in the
state, so neither `plan` nor `destroy` sees it (see [Teardown](#teardown)).
A deployment that has to be reproducible would manage the Helm release with
Terraform's `helm` provider or a GitOps controller, in separate staging and
production roots with remote state.

**Cost while running:** control plane ~$0.10/h, node ~$0.09/h, plus ~$0.03/h
per LoadBalancer Service (two here). An AWS Budgets alert
(`monthly_budget_usd`, default 50 USD) mails `budget_alert_emails` at 80% of
actual and 100% of forecast monthly spend. It covers the whole account on
purpose: what outlives a teardown (load balancers, EBS volumes) is exactly
what a budget filtered on this project's tags would miss. Mind the Kubernetes version: clusters
on versions past their standard-support window are billed AWS *extended
support* (~6× the control-plane rate). Check what's current:

```bash
aws eks describe-cluster-versions \
  --query 'clusterVersions[].{v:clusterVersion,status:versionStatus}'
```

## Images

The Helm chart defaults to `ghcr.io/abd-ulbasit/pgoverlay-branchd` at the
chart's `appVersion` (`image.tag` is empty, meaning "follow the chart"), so a
plain `helm install` pulls a published image (multi-arch from v1.0.0, so
Graviton nodes work too). To run your own build — a fork, a patch, or an image
mirrored into a registry inside the VPC — override both halves as the
`helm install` below does, and note the practical traps:

- **Building on Apple Silicon for x86 nodes** works with
  `docker buildx build --platform linux/amd64 -t <image> .` (and
  `-f Dockerfile.ghook` for the webhook image): the Dockerfiles run the Go
  toolchain on the build machine's own platform and cross-compile, so nothing
  runs under qemu emulation.
- **Nodes without Docker Hub access** (private subnets without NAT, a
  registry allow-list) also need the images branchd starts at runtime: set
  `helperImage` to a mirror of the utility helper image (see
  [Images](kubernetes.md#images)); the `postgres:<major>` images are pulled
  by that name, so they need a registry mirror configured on the nodes.
- **GHCR packages default to private.** Either make them public or create a
  pull secret and attach it to the service accounts (the chart's own SA for
  branchd, and `default` for branch/helper pods):

```bash
kubectl -n pgoverlay create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io --docker-username=<user> --docker-password=<token>
kubectl -n pgoverlay patch serviceaccount pgoverlay \
  -p '{"imagePullSecrets":[{"name":"ghcr-pull"}]}'
kubectl -n pgoverlay patch serviceaccount default \
  -p '{"imagePullSecrets":[{"name":"ghcr-pull"}]}'
```

## Deploy

> **Read this before putting the proxy on a public load balancer.** Every
> branch is a copy of production, and the proxy serves the Postgres wire
> protocol: without TLS the data crosses the internet in cleartext (clients
> on the default `sslmode=prefer` fall back to plaintext without a word), and
> without per-branch credentials a branch accepts the production password.
> The router also accepts connections before any authentication, so an
> endpoint open to the whole internet is denial-of-service surface. The
> walkthrough below therefore turns on TLS, per-branch credentials and a
> source-address allow-list. Even so, do not make branches that hold
> unmasked production data reachable from the internet: mask the source
> first (`pgb source set-mask`, see [usage](usage.md)), or keep the proxy
> internal and run the consumers in the VPC.

The default hostpath mode used here runs privileged pods (hostPath volumes,
`CAP_SYS_ADMIN` on branch pods), so the namespace must allow Pod Security
`privileged` ([csi mode](kubernetes.md#recommended-csi-mode) needs only
`baseline`):

```bash
kubectl create namespace pgoverlay
kubectl label namespace pgoverlay pod-security.kubernetes.io/enforce=privileged
```

**TLS for the proxy.** Clients will verify the proxy's certificate against a
name you control (here `pg.preview.example.com`, a CNAME to the proxy's load
balancer once it exists). Install cert-manager and issue that certificate
from a private CA (use an ACME issuer with a DNS-01 solver instead if you
want a publicly trusted certificate):

```bash
helm repo add jetstack https://charts.jetstack.io
helm install cert-manager jetstack/cert-manager -n cert-manager --create-namespace \
  --set crds.enabled=true
kubectl -n pgoverlay apply -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: selfsigned }
spec: { selfSigned: {} }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: pgoverlay-ca }
spec:
  isCA: true
  commonName: pgoverlay-ca
  secretName: pgoverlay-ca
  issuerRef: { name: selfsigned, kind: Issuer }
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: pgoverlay-ca }
spec: { ca: { secretName: pgoverlay-ca } }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: pgoverlay-proxy-tls }
spec:
  secretName: pgoverlay-proxy-tls
  dnsNames: ["pg.preview.example.com"]
  issuerRef: { name: pgoverlay-ca, kind: Issuer }
EOF
```

**Secrets, before the install.** Create them as Kubernetes Secrets from
files, and give the chart only their names. A value passed with `--set` is
stored in cleartext in the Helm release (itself a Secret in the namespace)
and printed by `helm get values`; `kubectl create secret --from-literal`
puts it in your shell history and the process list. The files below are
created owner-only and contain no trailing newline, which an HTTP
`Authorization` header would reject:

```bash
umask 077
# branchd's admin API token: keep it in your password manager as well
openssl rand -hex 32 | tr -d '\n' > api-token
kubectl -n pgoverlay create secret generic pgoverlay-token --from-file=token=api-token
# the webhook HMAC secret (you paste the same value into the GitHub webhook)
# and the GitHub App's private key (downloaded from the App's settings)
openssl rand -hex 32 | tr -d '\n' > webhook-secret
kubectl -n pgoverlay create secret generic pgoverlay-ghook \
  --from-file=webhook-secret=webhook-secret \
  --from-file=app-private-key=<app-name>.private-key.pem
rm api-token webhook-secret <app-name>.private-key.pem   # once stored safely
```

ghook authenticates to GitHub as a [GitHub App](github-app.md): its
installation tokens expire within an hour and reach only the repositories
the App is installed on. If you must use a token instead, make it a
fine-grained personal access token for that one repository (Pull requests:
read, Commit statuses: write, Issues: write), store it under the key
`github-token` instead of `app-private-key`, and leave `ghook.appId` unset.
Never hand it `gh auth token`: that is your CLI's OAuth token, with every
scope your account granted it.

**Install.** `proxy.service.loadBalancerSourceRanges` is the allow-list of
client networks (your CI runners' egress, an office or VPN range);
`ghook.service.loadBalancerSourceRanges` admits only GitHub's webhook
senders (IPv4 ranges from `api.github.com/meta`). To pin the images by
content, use `image.digest` and `ghook.image.digest` instead of the tags
([Images](kubernetes.md#images)).

```bash
helm install pgoverlay deploy/helm/pgoverlay -n pgoverlay \
  --set node=<storage-node-name> \
  --set image.repository=ghcr.io/<user>/pgoverlay-branchd --set image.tag=<tag> \
  --set existingSecret=pgoverlay-token \
  --set rotateBranchCredentials=true \
  --set limits.maxBranches=20 --set limits.maxTTL=168h \
  --set proxy.service.type=LoadBalancer \
  --set proxy.tls.certSecret=pgoverlay-proxy-tls \
  --set 'proxy.service.loadBalancerSourceRanges={<ci-egress-cidr>,<vpn-cidr>}' \
  --set ghook.enabled=true \
  --set ghook.image.repository=ghcr.io/<user>/pgoverlay-ghook --set ghook.image.tag=<tag> \
  --set ghook.existingSecret=pgoverlay-ghook --set ghook.appId=<app-id> \
  --set ghook.source=prod --set ghook.resetOnPush=true --set ghook.requireMask=true \
  --set ghook.repos=<owner>/<repo> \
  --set ghook.service.type=LoadBalancer \
  --set "ghook.service.loadBalancerSourceRanges={$(curl -s https://api.github.com/meta \
    | jq -r '.hooks | map(select(contains(":") | not)) | join(",")')}"
```

`ghook.requireMask` makes ghook refuse to branch `prod` for a pull request
until it has masking scripts, so seed and mask the source (below) before the
first PR.

`type: LoadBalancer` on EKS provisions Classic ELBs out of the box (raw TCP
— exactly what the wire-protocol proxy needs; no aws-load-balancer-controller
required), and `loadBalancerSourceRanges` becomes the ELB's security-group
rules. Platforms without fixed egress addresses (Vercel without Secure
Compute, for one) cannot be allow-listed; for those, TLS with `verify-full`
and per-branch credentials are what protect the endpoint, which is one more
reason to mask the source. To keep the proxy off the internet entirely, add
`--set proxy.service.annotations."service\.beta\.kubernetes\.io/aws-load-balancer-internal"=true`.

Once the proxy ELB has a hostname, point `pg.preview.example.com` at it (a
CNAME) and feed the name back so PR comments show the right address:

```bash
kubectl -n pgoverlay get svc pgoverlay-proxy \
  -o jsonpath='{.status.loadBalancer.ingress[0].hostname}'   # CNAME target
helm upgrade pgoverlay deploy/helm/pgoverlay -n pgoverlay --reuse-values \
  --set ghook.proxyHost=pg.preview.example.com:6432
```

Clients connect through the proxy with `dbname@branch`, verifying the
certificate against the CA:

```bash
kubectl -n pgoverlay get secret pgoverlay-ca -o jsonpath='{.data.ca\.crt}' | base64 -d > pgoverlay-ca.crt
psql "host=pg.preview.example.com port=6432 dbname=app@gh-d782c8-pr-42 user=app sslmode=verify-full sslrootcert=pgoverlay-ca.crt"
# gh-<repo-key>-pr-<number>; see docs/github-app.md#branch-names
```

**Give ghook its own token.** The install above hands ghook branchd's admin
token (the chart's NOTES warn about it). ghook only creates, resets and
destroys branches, so swap in an operator-role token:

```bash
kubectl -n pgoverlay port-forward svc/pgoverlay-api 7070 &
export PGOVERLAY_SERVER=http://localhost:7070
export PGOVERLAY_TOKEN="$(kubectl -n pgoverlay get secret pgoverlay-token -o jsonpath='{.data.token}' | base64 -d)"
# the operator token goes straight from pgb into the Secret, never to the screen
pgb token create ghook --role operator | tr -d '\n' |
  kubectl -n pgoverlay create secret generic pgoverlay-ghook-api --from-file=token=/dev/stdin
helm upgrade pgoverlay deploy/helm/pgoverlay -n pgoverlay --reuse-values \
  --set ghook.apiTokenSecret=pgoverlay-ghook-api
```

Point the GitHub webhook at
`http://<ghook-elb>:8080/webhook` (`pull_request` events, the
`webhook-secret` value from above).
Deliveries are HMAC-verified, and the allow-list keeps everyone but GitHub
off the endpoint; put an HTTPS ingress in front if the PR metadata in the
payloads should not travel in cleartext. Seed the source the native way
(`pgb source add` against the in-cluster service via the port-forward of
`pgoverlay-api` above), then mask it (`pgb source set-mask prod mask.sql`,
see [usage](usage.md)); if you enable `networkPolicy`, set
`networkPolicy.sourceEgress` to the source so the seed helpers can reach it
and nothing else.

## Upgrading Kubernetes

EKS moves one minor version at a time. `cluster_version` is a Terraform
variable for exactly this: plan one step, read it, apply that plan, then do
the next version.

```bash
v=1.34   # then 1.35, then 1.36: one minor version per step
terraform plan -out "k8s-$v.tfplan" -var cluster_version=$v \
  -var 'budget_alert_emails=["you@example.com"]'
terraform apply "k8s-$v.tfplan"
```

Do not script it with `-auto-approve`: each step replaces the node, and with
it (below) every hostpath branch and the registry. The plan should change
the control-plane version and the node group and nothing else; anything more
means the configuration drifted and is worth stopping for.

Each step upgrades the control plane (~10 min) and rolls the node group.
pgoverlay itself is indifferent — it uses only stable v1 APIs — but
**hostpath mode keeps all CoW data and the registry on the storage node's
disk, and a node rollover recycles that disk**. It also pins branchd to that
node by name, so once the node is replaced branchd stays Pending until you
point it at the new one. Branches are disposable by design, so the procedure
is: upgrade, `helm upgrade pgoverlay deploy/helm/pgoverlay -n pgoverlay
--reuse-values --set node=<new-node-name>`, then re-seed sources and let the
webhook recreate PR branches (or `pgb branch create` what you need). If
branch survival across node loss matters, use `storage.mode=csi` — PVC
clones live in EBS, not on the node, the registry moves to a PVC too, and
branchd is not pinned, so it comes back on a new node by itself.

## Teardown

```bash
kubectl -n pgoverlay delete svc pgoverlay-proxy pgoverlay-ghook   # release the ELBs
kubectl -n pgoverlay delete pvc --all                              # csi mode: release the EBS volumes
terraform -chdir=deploy/terraform/eks destroy \
  -var 'budget_alert_emails=["you@example.com"]'
```

`destroy` removes only what is in the state. Check that nothing Kubernetes
created outlived it: the in-tree cloud provider tags the load balancers and
security groups it creates, and the EBS CSI driver its volumes, with the
cluster name.

```bash
aws resourcegroupstaggingapi get-resources --region ap-south-1 \
  --tag-filters Key=kubernetes.io/cluster/pgoverlay \
  --query 'ResourceTagMappingList[].ResourceARN'
aws ec2 describe-volumes --region ap-south-1 \
  --filters Name=tag-key,Values=kubernetes.io/cluster/pgoverlay \
  --query 'Volumes[].VolumeId'
```

Both should print `[]`. Then remove what pointed at the cluster from
outside: the GitHub webhook to the ghook load balancer, CI variables and
secrets holding its addresses or tokens, and preview-platform environment
variables. Destroying the budget with the cluster is deliberate; keep an
account-level budget of your own.

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
   the branch — gate the job on the `pgoverlay/branch` commit status, which
   turns `success` only once the branch is ready, or retry the connection as
   the [demo repo's workflow](https://github.com/abd-ulbasit/pgoverlay-demo/blob/main/.github/workflows/pr-db-check.yml)
   does.
