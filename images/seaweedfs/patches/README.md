# Patches

## 001-fix-cve.patch

Fix CVE: bump vulnerable (transitive) dependencies of SeaweedFS to patched
versions (golang.org/x/crypto, x/net, x/sys, x/oauth2, x/image,
google.golang.org/grpc, go.opentelemetry.io/otel, go-jose, mongo-driver,
go-redis, jwt, edwards25519) so the resulting `weed` binary passes the CVE scan.
