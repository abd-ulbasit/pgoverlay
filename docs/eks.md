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
| seeding | `--via dump` (managed clouds block basebackup) | `pg_basebackup` from the in-cluster replica/primary |
| endpoints in CI/Vercel | re-wired on every tunnel restart | set once |

## Provision

`deploy/terraform/eks` holds a minimal single-node cluster (default VPC,
one `t3.large`, managed node group):

```bash
cd deploy/terraform/eks
terraform init && terraform apply
aws eks update-kubeconfig --name pgoverlay --region ap-south-1
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

The Helm chart defaults to `ghcr.io/abd-ulbasit/pgoverlay-branchd` at the
chart's `appVersion` (`image.tag` is empty, meaning "follow the chart"), so a
plain `helm install` pulls a published image. To run your own build — a fork, a
patch, or an image mirrored into a registry inside the VPC — override both
halves as the `helm install` below does, and note two practical traps:

- **Cross-compile on the host** (`GOOS=linux GOARCH=amd64 CGO_ENABLED=0`,
  pure-Go thanks to modernc.org/sqlite) and build a copy-only image.
  Running the Go toolchain under qemu emulation on Apple Silicon segfaults.
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

**Install.** `proxy.service.loadBalancerSourceRanges` is the allow-list of
client networks (your CI runners' egress, an office or VPN range);
`ghook.service.loadBalancerSourceRanges` admits only GitHub's webhook
senders (IPv4 ranges from `api.github.com/meta`):

```bash
helm install pgoverlay deploy/helm/pgoverlay -n pgoverlay \
  --set node=<storage-node-name> \
  --set image.repository=ghcr.io/<user>/pgoverlay-branchd --set image.tag=<tag> \
  --set token=$(openssl rand -hex 16) \
  --set rotateBranchCredentials=true \
  --set proxy.service.type=LoadBalancer \
  --set proxy.tls.certSecret=pgoverlay-proxy-tls \
  --set 'proxy.service.loadBalancerSourceRanges={<ci-egress-cidr>,<vpn-cidr>}' \
  --set ghook.enabled=true \
  --set ghook.image.repository=ghcr.io/<user>/pgoverlay-ghook --set ghook.image.tag=<tag> \
  --set ghook.webhookSecret=$(openssl rand -hex 16) \
  --set ghook.githubToken=<token-with-issues-write> \
  --set ghook.source=prod --set ghook.resetOnPush=true \
  --set ghook.repos=<owner>/<repo> \
  --set ghook.service.type=LoadBalancer \
  --set "ghook.service.loadBalancerSourceRanges={$(curl -s https://api.github.com/meta \
    | jq -r '.hooks | map(select(contains(":") | not)) | join(",")')}"
```

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
psql "host=pg.preview.example.com port=6432 dbname=app@gh-pr-42 user=app sslmode=verify-full sslrootcert=pgoverlay-ca.crt"
```

**Give ghook its own token.** The install above hands ghook branchd's admin
token (the chart's NOTES warn about it). ghook only creates, resets and
destroys branches, so swap in an operator-role token:

```bash
kubectl -n pgoverlay port-forward svc/pgoverlay-api 7070 &
PGOVERLAY_SERVER=http://localhost:7070 PGOVERLAY_TOKEN=<admin token> \
  pgb token create ghook --role operator                # prints the token once
kubectl -n pgoverlay create secret generic pgoverlay-ghook-api --from-literal=token=<it>
helm upgrade pgoverlay deploy/helm/pgoverlay -n pgoverlay --reuse-values \
  --set ghook.apiTokenSecret=pgoverlay-ghook-api
```

Point the GitHub webhook at
`http://<ghook-elb>:8080/webhook` (`pull_request` events, the same secret).
Deliveries are HMAC-verified, and the allow-list keeps everyone but GitHub
off the endpoint; put an HTTPS ingress in front if the PR metadata in the
payloads should not travel in cleartext. Seed the source the native way
(`pgb source add` against the in-cluster service via the port-forward of
`pgoverlay-api` above); if you enable `networkPolicy`, set
`networkPolicy.sourceEgress` to the source so the seed helpers can reach it
and nothing else.

## Upgrading Kubernetes

EKS moves one minor version at a time. `cluster_version` is a Terraform
variable for exactly this:

```bash
for v in 1.33 1.34 1.35 1.36; do
  terraform apply -auto-approve -var cluster_version=$v
done
```

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
   [demo repo's workflow](https://github.com/abd-ulbasit/pgoverlay-demo/blob/main/.github/workflows/pr-db-check.yml).
