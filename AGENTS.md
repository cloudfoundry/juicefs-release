# Development loop

This is the `juicefs` BOSH release. `src/juicefs` is a pinned upstream Git submodule;
`packages/juicefs` builds it with the vendored `golang-1.25-linux` BOSH package.
`nginx_webdav` is vendored from CAPI release and powers our own singleton
`blobstore` job; re-vendor it from `../capi-release` when upgrading CAPI's package.
The vendored package locks live in `packages/nginx_webdav/spec.lock`,
`packages/golang-1.25-linux/spec.lock`, and `packages/cf-cli-8-linux/spec.lock`.
Their build indexes in `.final_builds/` and package blobs in `final_blobs/` are
local and ignored by Git.
Run `git submodule update --init --recursive` after cloning. Regenerate the local
Go module vendor tree after changing the upstream revision (`devbox run vendor-go`).
The vendor tree is staged under `src/juicefs-deps` so it is shipped to compilation
VMs; compilation has no outbound Go module dependency.

For local POC releases, `config/final.yml` uses a local blobstore in `final_blobs/`.
`blobs/`, `final_blobs/`, and `.final_builds/` are ignored by Git. Keep local
blob data and build indexes when rebuilding. A fresh clone needs both the local
blobstore and vendored-package build indexes restored, or the packages re-vendored
from their source releases with `bosh vendor-package`, before it can build this
release. A later permanent blobstore can replace the local one. Do not enable Git LFS.

`bosh.env` is a local, ignored copy of `../instant-bosh/bosh.env`. It contains
director credentials. Do not commit it or print its contents. Refresh it from
the instant-bosh repo when the director credentials change.

From the release root:

```sh
devbox run deploy
```

This sources `bosh.env`, runs `bosh create-release --force`, uploads that release,
updates the named `juicefs` runtime-config with `bosh update-config` using the
newly built release's explicit version, deploys the JuiceFS release, then runs
`bosh deploy -n -d cf <(bosh manifest -d cf)` so pending runtime-config changes
are applied. The JuiceFS manifest contains
`juicefs-backends` and `juicefs-broker` instance groups and uses `version: latest` for the
`juicefs` release. The director must have an
`ubuntu-noble` stemcell and cloud-config with `z1`, `default` network and
`default` VM type. Postgres and BPM are pinned external releases; BPM is used by
the blobstore job. The JuiceFS FUSE client runs directly on the VM under Monit,
not in BPM, so it can access the host `/dev/fuse` and publish its mount normally.
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
in `pre-start` on `spec.bootstrap` before mounting (`juicefs.volume.initialize: true`). Existing filesystems are
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
This allows bootstrap initialization to connect to the backends during `pre-start`.
The CF runtime-config targets the
live `control` group for the full-root mount and `compute` for the isolated
volume driver. `juicefs-volume` is a foreground FUSE client managed
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

`operations/enable-juicefs-cf-volume-service.yml` enables CAPI volume services
and defines registration/deregistration errands. The CF deployment is separate
from the JuiceFS deployment. The JuiceFS deployment marks Postgres, WebDAV, the
full-root mountpoint, and broker as shared link providers. The CF runtime-config
consumes the broker mounter's shared `juicefs_mount` link from deployment
`juicefs`. That mounter is authoritative for `juicefs.metadata` (postgres or
mysql), `juicefs.storage` (webdav or s3), and the filesystem name. The CF mount
and Diego driver are link-only, with no independent backend overrides. Quota
commands reuse the mounter's rendered configuration. The colocated broker
consumes its mountpoint link. `operations/use-mysql-s3.yml` shows external
backend configuration; changing properties does not migrate existing data.

The runtime-config is `runtime-config/juicefs-volume-driver.yml`. It targets the
live `control` group for the full-root mount and `compute` for the isolated
volume driver. Broker registration errands consume the shared broker link. The
broker provisions
directories beneath `/cf-volumes/<service-instance-guid>`; each cell mount uses
JuiceFS `--subdir` for only that directory. Bindings are synchronous and stateless,
with `bindings_retrievable: false`; binding retrieval is not implemented.
The optional binding parameter `mount` selects a clean absolute container path
(for example, `cf bind-service cf-env foobar -c '{"mount":"/data"}'`). Without it,
the path is `/var/vcap/data/<service-instance-guid>`. Cloud Controller stores the
returned mount instructions. This view restriction is not strong
isolation against hostile users who can access shared backend credentials.
The broker-registration errands currently use the configured CF admin
credential; replace that with a dedicated account authorized to manage service
brokers and service access before using this POC beyond a test environment.

Build and validate from this release root with `bosh create-release --force`,
then interpolate the CF manifest with the ops file and the runtime-config with
the required backend variables. After applying both to the director,
run `register-juicefs-broker`, then verify CAPI provision/bind and app-container
read/write/unmount behavior before relying on deprovision cleanup.
