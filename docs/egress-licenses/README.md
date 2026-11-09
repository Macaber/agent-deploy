# Proxy guard and sync development artifacts

Not a production release or a completed license audit.

Envoy base: envoyproxy/envoy:v1.40.2
Digest: sha256:8de97e654644827d62afdbf7e1d3dc9fee01a8690d410dd1c63a30dc6b6cecbb
Envoy main project: Apache-2.0. The Ubuntu image and bundled libraries have additional licenses.

Guard: adds iptables 1.8.7-1ubuntu5.2 and its dependencies. The package inventory
and installed copyright files are adjacent to this document. They include GPL,
LGPL and other license terms. Do not classify this image as Apache-2.0-only.
The sync image contains a statically linked Go program and a CA certificate
bundle from the pinned base. A complete Go dependency license/SBOM audit and
full base-image license inventory remain required before release.

Guard installation uses CAP_NET_ADMIN only in an init container, in the proxy
Pod network namespace (never hostNetwork). Envoy UID 101 and sync UID 102 drop
all runtime capabilities. The API token is mounted only in the sync container.
Missing guard inputs or command errors prevent proxy startup.

Protected CIDRs must cover all Workspace Pod and node management ranges, plus
cluster management services. They are DENY boundaries, not business grants.
Explicit DNS IPs are allowed only on TCP/UDP 53. IPv6 upstream is disabled and
UID 101 IPv6 initiation is rejected. Changes to infrastructure boundaries need
a planned proxy rollout; everyday business allowlist changes still use RDS.

Local tests completed:
- guard initialized twice successfully in the dedicated test network namespace;
- UID 102 connected to the live Envoy 127.0.0.1:9901 admin listener;
- UID 101 was rejected for that listener, 169.254.169.254:80 and 10.1.2.3:80.
These socket checks do not replace DNS rebinding, HTTP/CONNECT or Calico tests.
