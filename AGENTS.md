# Development loop

When changing documented commands, paths, or development constraints, update this
file in the same change. Verify claims against code; link to detailed documentation
rather than duplicating it.

This is the `juicefs` BOSH release. `src/juicefs` is a pinned upstream Git submodule;
`packages/juicefs` builds it with the vendored `golang-1.25-linux` BOSH package.
`nginx_webdav` is vendored from CAPI release and powers our own singleton
`blobstore` job; re-vendor it from `../capi-release` when upgrading CAPI's package.
The vendored package locks live in `packages/nginx_webdav/spec.lock`,
and `packages/golang-1.25-linux/spec.lock`.
Their build indexes in `.final_builds/` are tracked; package blobs are stored in
the GCS release blobstore.
Run `git submodule update --init --recursive` after cloning. Upstream Go submodules use
`pre_packaging` hooks to run `go mod vendor` in BOSH's temporary source copy.
Use Go 1.25, Git, and OpenSSL for local release creation; Devbox can provide these
tools but is optional. Run `bosh create-release --force` when they are already on
`PATH`, or `devbox run -- bosh create-release --force` to use the declared versions.
Release creation needs module network access or a populated module cache;
compilation VMs receive the generated vendor trees and build offline.
Release-owned Go modules check in their vendor directories, go.mod/go.sum,
and Counterfeiter-generated fakes. Use Ginkgo v2/Gomega for behavioral suites.
Run `scripts/test-go` for randomized race tests and coverage excluding fakes;
run `scripts/generate-fakes` to regenerate fakes using the pinned vendored tool.
`juicefs-acceptance-tests` packages a static test app and a compiled Ginkgo BOSH
errand. Local checks include a TLS CF API fixture; the live suite requires
`JUICEFS_ACCEPTANCE_CONFIG`. CI orchestration lives in `scripts/acceptance/run`;
see `docs/acceptance-testing.md`. The workflow currently uses the standard
GitHub-hosted `ubuntu-latest` runner. Keep instant-bosh CLI/image pins together
in `scripts/acceptance/versions.env`. Run the orchestration only on disposable hosts.
Do not check generated vendor trees for upstream submodules into Git.
`packages/juicefs` contains only the upstream client. Release-owned tools have
separate packages and Go modules under matching `src/` directories; these compile
offline against their checked-in vendor trees. Both broker
registration errands use the single `broker-registration` binary/package.
Registration uses the vendored go-cfclient v3 API client, not a packaged CF CLI;
wait for asynchronous broker jobs and scope catalog queries to the broker GUID.
`src/azure-postgres-auth-proxy` is pinned to v0.1.5.
`src/certifi` supplies packaged public CA roots. The optional Azure proxy uses
BPM, listens only on loopback, and publishes a local-only `azure_postgres_proxy`
link. See `docs/azure-managed-identities.md` for Azure-specific deployment commands.

`config/final.yml` uses GCS bucket `juicefs-release-blobs`. Configure uploader
credentials in ignored `config/private.yml`; never print or commit its contents.
`blobs/` and legacy `final_blobs/` are ignored caches; keep their local data when
rebuilding. Vendored-package indexes in `.final_builds/` must be tracked alongside
`spec.lock` files. Run `bosh sync-blobs` after cloning; run `bosh upload-blobs`
after adding blobs. The repository home is
`https://github.com/cloudfoundry/juicefs-release`. Do not enable Git LFS.

`bosh.env` is a local, ignored copy of `../instant-bosh/bosh.env`. It contains
director credentials. Do not commit it or print its contents. Refresh it from
the instant-bosh repo when the director credentials change.

From the release root, run `devbox run deploy` for the instant-bosh validation
environment. `scripts/deploy` defines its deployment sequence. See
`README.md#deploy-and-register` and `docs/acceptance-testing.md` for details.

The base `deployment/juicefs.yml` accepts `juicefs_metadata` and
`juicefs_storage` objects. `deployment/operations/non-ha-validation.yml` adds
singleton Postgres/WebDAV, their DNS aliases, and the 50 GiB backend disk.
Instant-bosh DNS and credential adjustments live in
`deployment/operations/misc/`; other directors should use their normal DNS and
credential configuration. The director needs an `ubuntu-noble` stemcell and
cloud-config with `z1`, the `default` network, and `default` VM type. The JuiceFS
FUSE client runs directly on the VM under Monit so it can access host `/dev/fuse`.
The singleton
WebDAV blobstore keeps objects on its persistent disk at
`/var/vcap/store/juicefs-blobstore/`. The job provides a `webdav` BOSH link;
future JuiceFS clients should consume that link rather than hardcoding an address.
The authenticated WebDAV endpoint is `https://<link address>:<blobstore.port>/admin/`;
the `blobstore.admin_users` and `blobstore.tls.cert` link properties provide its
credentials and certificate. Keep `blobstore` at one instance: each instance's
persistent disk holds separate objects, so scaling it would not create shared storage.

JuiceFS metadata lives in Postgres, data blocks live in the WebDAV blobstore,
and the singleton mount initializes missing metadata using `format --no-update`
in its supervised mount launcher on `spec.bootstrap` before mounting (`juicefs.volume.initialize: true`). Existing filesystems are
left unchanged; backend errors fail startup. CF mounts leave initialization
disabled. Initialization cannot use `post-start`, because BOSH requires the
mount and broker to be healthy before running that hook.
Each mount has its own ephemeral local cache.
All mounts of this volume
must use those same backends. Clients connect directly to
`https://blobstore.service.juicefs.internal:4443/admin/`, resolved using BOSH DNS
aliases targeting `juicefs-backends`. A separate CA signs the server certificate
with a matching DNS SAN; the linked CA is rendered to each client and passed via
`SSL_CERT_FILE`. The filesystem's stored endpoint was migrated from loopback
using `juicefs config --bucket`; no local WebDAV proxy is needed.

The JuiceFS deployment runs Postgres and WebDAV in `juicefs-backends`, separately
from the broker and full-root `juicefs-volume` mount in `juicefs-broker`.
This allows bootstrap initialization to connect to the backends. The launcher
also allows a colocated Azure PostgreSQL auth proxy to start concurrently.
The default CF runtime-config targets upstream cf-deployment's `diego-cell` for
the isolated volume driver; instant-bosh compatibility targets `compute`.
`juicefs-volume` is a foreground FUSE client managed
directly by Monit. Its
launcher checks the host `/dev/fuse` device and logs under
`/var/vcap/sys/log/juicefs-volume/`. JuiceFS enables go-fuse direct mounting, so
it opens `/dev/fuse` and mounts without requiring `fusermount`. The earlier BPM-based attempt
failed to open `/dev/fuse` inside the container (`operation not permitted`);
running outside BPM avoids that isolated device namespace and makes the mount
visible in the host mount namespace.
The certificate variable uses `update_mode: converge` so a config server can
regenerate it when generation options change. This option is not honored when
using a local `--vars-store` with `bosh create-env`.

Postgres consumers request IP addresses from their links. WebDAV uses the stable
DNS alias and normal CA/hostname verification. The previous self-signed CA-style
blobstore certificate lacked SANs; a separate CA and server certificate fixed
generation, and BOSH DNS is now installed on JuiceFS VMs. The earlier CAPI blobstore job returned HTTP 405 for
`MKCOL`; that is why this release provides its own WebDAV job using CAPI's
vendored `nginx_webdav` package.
On the deployed POC, `verify-backends` exits 0: Postgres `SELECT 1`, WebDAV
`PROPFIND`, `PUT`, `GET`, `MKCOL`, and cleanup `DELETE` all succeed.

## Cloud Foundry volume service POC

`juicefs.broker.plans` configures stable plan IDs, CF-facing names, descriptions,
`free`, `capacity_gib`, and `inodes`. Zero limits mean unlimited. The manifest
keeps the original unlimited `shared` plan and adds an example `small` plan with
1 GiB and 1000 inodes. Quotas are directory usage limits, not backend disk
reservations. JuiceFS enforces them across all mounts of the service instance.
Plan changes apply to new instances only; upgrades/downgrades are disabled.
Provisioning records the selected plan and limits under
`/.juicefs-broker/instances/<guid>.json`, outside the app-mounted subtree. Pending
records make failed quota application retryable; completed provision retries do
not change quotas. Existing pre-quota directories retain the unlimited shared
plan. The broker's instance metadata lives on JuiceFS, not on its ephemeral disk.

See `README.md#cloud-foundry-volume-service-poc` and
`docs/acceptance-testing.md` for the volume-service deployment and validation
details. `scripts/deploy` is the source of truth for the instant-bosh deployment
sequence. The CF runtime-config consumes the broker mounter's shared
`juicefs_mount` link; the mounter defines the backend configuration and
filesystem name. The driver mounts only the service-instance subdirectory.
Deployment ops/runtime-config target coverage is in `deployment/tests`; run
`go test ./...` there with BOSH CLI installed.

Service bindings are synchronous and stateless; binding retrieval is not
implemented. The optional `mount` binding parameter selects an absolute container
path, defaulting to `/var/vcap/data/<service-instance-guid>`. The broker currently
uses the configured CF administrator credential; replace it with a dedicated
broker-management account before using this POC beyond a test environment.
