# i2g — Ingress to Gateway API controller

A Kubernetes controller (built on [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime))
that watches `networking.k8s.io/v1` **Ingress** resources of a configurable
IngressClass in selected namespaces and translates each of them into Gateway API
resources:

- a **ListenerSet** (`gateway.networking.k8s.io/v1`) holding one HTTPS and one
  plain HTTP listener per `spec.tls` host, attached to a pre-existing,
  configurable **Gateway**
- one **HTTPRoute** per distinct host in `spec.rules`

All writes use **server-side apply** (typed apply configurations with field
manager `ingress2gateway`, force ownership), so the controller only owns the
fields it sets and converges drift automatically.

## Requirements

- Gateway API CRDs **v1.5+** (ListenerSet is part of the standard channel since v1.5)
- A Gateway whose `spec.allowedListeners` admits ListenerSets from the Ingress
  namespaces, e.g.:

  ```yaml
  apiVersion: gateway.networking.k8s.io/v1
  kind: Gateway
  metadata:
    name: shared-gateway
    namespace: infra
  spec:
    gatewayClassName: example
    listeners:
      - name: http
        port: 80
        protocol: HTTP
        allowedRoutes:
          namespaces:
            from: All        # let HTTPRoutes from app namespaces attach
    allowedListeners:
      namespaces:
        from: All            # let ListenerSets from app namespaces attach
  ```

- Go 1.26+ to build: `go build -o bin/i2g .`

## Flags

| Flag | Required | Default | Description |
|---|---|---|---|
| `--ingress-class` | no | all classes | IngressClass to reconcile. Matched against `spec.ingressClassName`, falling back to the legacy `kubernetes.io/ingress.class` annotation. Empty considers all Ingresses in the watched/selected namespaces regardless of their class. |
| `--namespace-selector` | no | all namespaces | Label selector applied to **Namespace** labels, e.g. `team=platform,env in (prod,staging)`. Ingresses outside matching namespaces are ignored (and previously generated resources are removed when a namespace stops matching). Mutually exclusive with `--watch-namespaces`. |
| `--watch-namespaces` | no | all namespaces | Comma-separated list of namespaces to watch/reconcile. Allows running with purely namespaced RBAC — in this mode the controller never reads Namespace objects. Mutually exclusive with `--namespace-selector`. |
| `--gateway-name` | yes | – | Name of the Gateway the ListenerSets attach to and the HTTPRoutes reference as parent. |
| `--gateway-namespace` | no | Ingress namespace | Namespace of that Gateway. |
| `--default-cluster-issuer` | no | – | Value for `cert-manager.io/cluster-issuer` on the ListenerSet when the Ingress has `kubernetes.io/tls-acme: "true"` but no issuer annotation. |
| `--update-ingress-status` | no | `false` | Mirror the Gateway's `status.addresses` into `status.loadBalancer` of reconciled Ingresses so consumers like external-dns and `kubectl get ingress` keep working. Enable only once the original ingress controller no longer manages the Ingresses — two controllers writing the status fight each other. |
| `--listener-https-port` | no | `443` | Port of the generated HTTPS listeners. |
| `--listener-http-port` | no | `80` | Port of the generated plain HTTP listeners (e.g. for ACME HTTP-01 challenges). |
| `--metrics-bind-address` | no | `:8080` | Metrics endpoint (`0` disables). |
| `--health-probe-bind-address` | no | `:8081` | Health/readiness probes. |
| `--leader-elect` | no | `false` | Leader election. |

Plus the standard controller-runtime zap logging flags (`--zap-log-level`, …).

Example:

```sh
i2g --ingress-class=nginx \
    --namespace-selector='migrate-to-gateway=true' \
    --gateway-name=shared-gateway \
    --gateway-namespace=infra \
    --default-cluster-issuer=letsencrypt-prod
```

## Translation

Given this Ingress:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: shop
  namespace: team-a
  annotations:
    cert-manager.io/common-name: shop.example.com
    kubernetes.io/tls-acme: "true"
spec:
  ingressClassName: nginx
  tls:
    - hosts: [shop.example.com]
      secretName: shop-tls
  rules:
    - host: shop.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: frontend
                port:
                  name: http   # resolved to the numeric port via the Service
```

the controller applies:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: ListenerSet
metadata:
  name: shop                       # same name/namespace as the Ingress
  namespace: team-a
  annotations:
    cert-manager.io/common-name: shop.example.com
    cert-manager.io/cluster-issuer: letsencrypt-prod   # from --default-cluster-issuer
  labels:
    app.kubernetes.io/managed-by: ingress2gateway
spec:
  parentRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: shared-gateway
    namespace: infra
  listeners:
    - name: https-shop-example-com-<hash>
      hostname: shop.example.com
      port: 443
      protocol: HTTPS
      tls:
        mode: Terminate
        certificateRefs:
          - name: shop-tls
    - name: http-shop-example-com-<hash>   # plain HTTP, e.g. for ACME HTTP-01
      hostname: shop.example.com
      port: 80
      protocol: HTTP
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: shop-shop-example-com-<hash>   # one HTTPRoute per distinct rule host
  namespace: team-a
  labels:
    app.kubernetes.io/managed-by: ingress2gateway
spec:
  parentRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: shared-gateway
      namespace: infra
    - group: gateway.networking.k8s.io   # added because spec.tls covers the host
      kind: ListenerSet
      name: shop
  hostnames: [shop.example.com]
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - name: frontend
          port: 8080
```

### Rules of the translation

- **pathType**: `Exact` → `Exact`; `Prefix`, `ImplementationSpecific`, and
  unset all map to `PathPrefix`. An empty path becomes `/`.
- **Named service ports** are resolved to numbers by reading the backend
  Service. Services are watched, so port renumbering re-renders the routes.
  An unresolvable port fails the reconcile and is retried with backoff.
- **TLS**: one HTTPS listener plus one plain HTTP listener (so e.g. ACME
  HTTP-01 challenges can succeed) per host of each `spec.tls` entry; an entry
  without hosts becomes a catch-all listener pair (no `hostname`). Entries
  without `secretName` are skipped. HTTPRoutes for hosts covered by TLS
  (exact or single-label wildcard match) additionally attach to the
  ListenerSet.
- **HTTPS redirect**: hosts covered by `spec.tls` answer plain HTTP with a
  permanent (308) redirect to HTTPS, like ingress-nginx: the app route is
  pinned to the host's HTTPS listener (`sectionName`) and a separate
  generated `<route>-redirect` HTTPRoute with a `RequestRedirect` filter
  attaches to the HTTP listener. Set
  `nginx.ingress.kubernetes.io/ssl-redirect: "false"` (or the legacy
  `ingress.kubernetes.io/ssl-redirect`) on the Ingress to serve the
  application on both protocols instead. ACME HTTP-01 keeps working: solver
  routes match a more specific path than the redirect's `/` prefix.
- **`spec.defaultBackend`** becomes a hostname-less `PathPrefix /` rule with
  the lowest matching precedence: Gateway API prefers routes with more
  specific hostnames and paths, and within the shared catch-all route the
  rule is appended last, so all explicit rules win — mirroring Ingress
  default-backend semantics.
- **cert-manager annotations**: every `cert-manager.io/*` annotation of the
  Ingress is copied to the ListenerSet. If neither
  `cert-manager.io/cluster-issuer` nor `cert-manager.io/issuer` is present but
  `kubernetes.io/tls-acme: "true"` is, `cert-manager.io/cluster-issuer` is set
  to `--default-cluster-issuer`. No other annotations or labels are copied.
- **Ownership & cleanup**: generated resources carry an owner reference to
  the Ingress (deleting the Ingress garbage-collects them) and the
  `app.kubernetes.io/managed-by: ingress2gateway` label. HTTPRoutes for hosts
  removed from the Ingress, and the ListenerSet when TLS is removed, are
  deleted explicitly; the same happens when the Ingress stops matching the
  class or namespace selector.

### Out of scope

- `backend.resource` (non-Service backends)
- Ingress rules without an `http` section

## Deploying

### Helm

The chart lives in [charts/i2g](charts/i2g) and is published as an OCI
artifact together with the image by the
[Build and Publish workflow](.github/workflows/build-and-publish.yaml):

```sh
helm install i2g oci://ghcr.io/speer/charts/i2g \
  --namespace i2g-system --create-namespace \
  --set controller.ingressClass=nginx \
  --set controller.gatewayName=shared-gateway \
  --set controller.gatewayNamespace=infra \
  --set controller.defaultClusterIssuer=letsencrypt-prod
```

`controller.gatewayName` is required; see
[values.yaml](charts/i2g/values.yaml) for all options (namespace selector,
listener ports, leader election, metrics, resources, …). RBAC and the
ServiceAccount are created by the chart.

By default the controller gets cluster-wide permissions. With
`rbac.namespaced=true` no cluster-wide permissions are granted at all: a
Role/RoleBinding is created in every watched namespace
(`controller.watchNamespaces`, defaulting to the release namespace) and the
controller is started with `--watch-namespaces` accordingly. Because
namespaces are then fixed, `controller.namespaceSelector` cannot be combined
with this mode (the chart rejects it at render time).

### Docker image

The [Dockerfile](Dockerfile) builds a static binary into a distroless
non-root image:

```sh
docker build -t i2g .
```

### Plain manifests

RBAC for the controller's ServiceAccount is in [deploy/rbac.yaml](deploy/rbac.yaml)
(read Ingresses/Services/Namespaces cluster-wide, manage ListenerSets and
HTTPRoutes, plus a leader-election Role).

## Releases

Pushes to `main` run tests, then [cocogitto](https://github.com/cocogitto/cocogitto)
derives the next version from conventional commits, tags it, and publishes
`ghcr.io/<owner>/i2g:<version>` and the Helm chart
`oci://ghcr.io/<owner>/charts/i2g`. Dependencies are kept current by
[Renovate](.github/renovate.json).

## Development

```sh
go build ./...
go test ./...
go run . --ingress-class=nginx --gateway-name=shared-gateway --gateway-namespace=infra
```
