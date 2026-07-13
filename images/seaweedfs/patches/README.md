# Patches

No patches are currently applied.

`001-fix-cve.patch` (bumping vulnerable transitive deps: golang.org/x/crypto,
x/net, x/sys, x/oauth2, x/image, google.golang.org/grpc, go.opentelemetry.io/otel,
go-jose, mongo-driver, go-redis, jwt, edwards25519) was dropped when SeaweedFS was
bumped to 4.39: upstream `go.mod` already pins those deps at equal-or-newer
(patched) versions, so the patch is unnecessary and no longer applies cleanly.

If a future CVE scan flags a transitive dep, add a `*.patch` here bumping it; the
build loop in `werf.inc.yaml` applies every `*.patch` in this directory (and is a
no-op when there are none).
