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
| `--warn-annotation-prefixes` | no | `nginx.ingress.kubernetes.io/,ingress.kubernetes.io/` | Comma-separated annotation prefixes that carried traffic semantics for the previous ingress controller. Untranslated annotations with these prefixes raise a warning Event on the Ingress. Empty disables the warnings. |
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
    --gateway-namespace=infra
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
    cert-manager.i2g.dev/cluster-issuer: letsencrypt-prod   # becomes cert-manager.io/cluster-issuer
    cert-manager.i2g.dev/common-name: shop.example.com
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
    cert-manager.io/cluster-issuer: letsencrypt-prod
    cert-manager.io/common-name: shop.example.com
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
    - group: gateway.networking.k8s.io   # added because spec.tls covers the host;
      kind: ListenerSet                  # pinned to the HTTPS listener because of
      name: shop                         # the default HTTPS redirect
      sectionName: https-shop-example-com-<hash>
  hostnames: [shop.example.com]
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - name: frontend
          port: 8080
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: shop-shop-example-com-<hash>-redirect   # plain HTTP answers with 308 to HTTPS
  namespace: team-a
  labels:
    app.kubernetes.io/managed-by: ingress2gateway
spec:
  parentRefs:
    - group: gateway.networking.k8s.io
      kind: ListenerSet
      name: shop
      sectionName: http-shop-example-com-<hash>
  hostnames: [shop.example.com]
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      filters:
        - type: RequestRedirect
          requestRedirect:
            scheme: https
            statusCode: 308
```

### Rules of the translation

- **pathType**: `Exact` → `Exact`; `Prefix`, `ImplementationSpecific`, and
  unset all map to `PathPrefix`. An empty path becomes `/`. Path values pass
  through unchanged: Gateway API's `PathPrefix` is spec-defined as
  semantically equivalent to Ingress `Prefix` (element-wise matching,
  trailing `/` ignored), and `Exact` is exact in both APIs.
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
- **cert-manager annotations**: annotations prefixed `cert-manager.i2g.dev/`
  are written to the ListenerSet as `cert-manager.io/` annotations
  (`cert-manager.i2g.dev/cluster-issuer` → `cert-manager.io/cluster-issuer`,
  …). Plain `cert-manager.io/*` annotations are deliberately **not** copied:
  cert-manager's ingress-shim acts on them on the Ingress, and copying them
  would make cert-manager manage the same Certificate from both the Ingress
  and the ListenerSet (a warning Event with the rename hint is raised
  instead). Renaming the annotation on the Ingress is therefore the
  per-Ingress certificate cutover: ingress-shim removes its Ingress-owned
  Certificate and cert-manager re-creates it owned by the ListenerSet.
  `kubernetes.io/tls-acme` is ignored entirely — set
  `cert-manager.i2g.dev/cluster-issuer` explicitly instead. No other
  annotations or labels are copied.
- **Feedback on skipped constructs**: anything the translation drops —
  non-Service backends, TLS entries without `secretName`, rules without an
  `http` section, and untranslated annotations under the
  `--warn-annotation-prefixes` namespaces — raises a warning **Event** on the
  source Ingress (visible in `kubectl describe ingress`; repeats are
  deduplicated into one event with a counter). Annotations outside those
  prefixes (Helm, GitOps tooling, …) are ignored silently, as they carry no
  traffic semantics. A clean Ingress emits nothing.
- **Feedback from the Gateway implementation**: after applying, the
  controller reads the status conditions the Gateway implementation writes
  on the generated resources and raises warning Events on the Ingress for
  definitive failures — ListenerSet/listener not `Accepted`/`Programmed`
  (e.g. the Gateway does not allow ListenerSets, listener limits, missing
  TLS Secret) and HTTPRoute parents not `Accepted`/`ResolvedRefs` (e.g.
  hostname/listener mismatch, backend Service gone). This catches
  "translated but not actually serving". `Unknown` conditions are ignored;
  they only mean the implementation has not processed the resource yet.
- **Opting out**: annotate an Ingress with `i2g.dev/skip: "true"` to exclude
  it from translation; previously generated resources (and, with
  `--update-ingress-status`, the controller's status ownership) are cleaned
  up. Removing the annotation re-translates it.
- **Same TLS host in multiple Ingresses**: each Ingress gets its own
  ListenerSet, so two Ingresses declaring TLS for the same host produce two
  listeners for that host:443. Per the Gateway API merge rules the oldest
  ListenerSet wins; the others get `Conflicted: True`, which this controller
  surfaces as a warning Event on the affected Ingress. This differs from
  ingress-nginx, which merges such Ingresses — consolidate the TLS section
  into one Ingress per host if you hit this.
- **Ownership & cleanup**: generated resources carry an owner reference to
  the Ingress (deleting the Ingress garbage-collects them) and the
  `app.kubernetes.io/managed-by: ingress2gateway` label. HTTPRoutes for hosts
  removed from the Ingress, and the ListenerSet when TLS is removed, are
  deleted explicitly; the same happens when the Ingress stops matching the
  class or namespace selector.

### Out of scope

- `backend.resource` (non-Service backends)
- Ingress rules without an `http` section
- `spec.tls` entries without `secretName`: ingress controllers fall back to
  their default certificate, which has no per-Ingress equivalent here. Such
  entries are skipped — configure a catch-all HTTPS listener with a default
  certificate on the shared Gateway instead if you need that behavior.
- Translating controller-specific annotations that configure traffic
  behavior (`rewrite-target`, canary weights, CORS, auth, rate limits, …).
  These raise warning Events instead (see `--warn-annotation-prefixes`),
  giving you a per-Ingress inventory of what must be ported by hand — e.g.
  to HTTPRoute filters or implementation policies.

## Migrating from an ingress controller

The controller is built to run **alongside** the existing ingress
controller; nothing is removed from it and the Ingresses are never modified
(except, optionally, their status). A typical migration:

1. **Install** Gateway API v1.5+ CRDs, a Gateway implementation, and a
   Gateway with `allowedListeners` admitting the app namespaces (see
   Requirements). Deploy i2g with the matching `--ingress-class`, scoped to
   a pilot set of namespaces via `--namespace-selector`.
2. **Review warnings**: `kubectl get events -A --field-selector
   reason=UnsupportedAnnotation` (and the other warning reasons) lists every
   Ingress relying on behavior the translation drops; port those to
   HTTPRoute filters or implementation policies by hand, or exclude the
   Ingress with `i2g.dev/skip: "true"` for now.
3. **Cut over certificates**: rename `cert-manager.io/*` annotations to
   `cert-manager.i2g.dev/*` on each Ingress (`CertManagerAnnotationIgnored`
   events list the affected ones), and replace any `kubernetes.io/tls-acme`
   with an explicit `cert-manager.i2g.dev/cluster-issuer: <issuer>`. This
   moves certificate management from the Ingress to the ListenerSet; the
   Secret keeps its name, so both data planes keep serving the same
   certificate.
4. **Verify per host** against the Gateway's address (e.g. `curl
   --resolve`): TLS, redirect, routing, ACME renewals. The generated
   resources' status failures show up as warning Events on the Ingresses.
5. **Cut over DNS** to the Gateway address. Once the old ingress controller
   no longer serves traffic, enable `--update-ingress-status` so
   external-dns and `kubectl get ingress` follow the Gateway, then
   decommission the old controller.
6. Widen `--namespace-selector` and repeat. Eventually, replace the
   Ingresses with native HTTPRoutes at your own pace — deleting an Ingress
   garbage-collects its generated resources.

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
  --set controller.gatewayNamespace=infra
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
