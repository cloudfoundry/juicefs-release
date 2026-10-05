# JuiceFS BOSH release

A proof-of-concept [JuiceFS](https://juicefs.com/) volume service for Cloud Foundry.
The release includes a service broker, a Diego volume driver, and a shared
filesystem with PostgreSQL or MySQL metadata and WebDAV or S3 object storage.
The supplied deployment uses PostgreSQL and persistent WebDAV storage.

Applications can provision a directory, bind it at a chosen container path, and
keep files across container restarts. BOSH-configurable service plans apply
directory capacity and inode quotas.

## Architecture

```text
Cloud Foundry CLI → Cloud Controller → JuiceFS service broker
                                           │
                                           └─ provisions /cf-volumes/<instance-guid>

Diego cell → JuiceFS volume driver → subdirectory FUSE mount → app container
                      │
                      ├─ PostgreSQL / MySQL: shared filesystem metadata
                      └─ WebDAV / S3: persistent data blocks
```

The `juicefs` deployment has two singleton instance groups:

| Group | Jobs | Storage |
| --- | --- | --- |
| `juicefs-backends` | PostgreSQL, BPM, WebDAV blobstore | Persistent disk for metadata and objects |
| `juicefs-broker` | Full-root JuiceFS mount, service broker | Ephemeral cache; instance records live on JuiceFS |

The backend group updates first, using BOSH's default serial instance-group
updates. The mount initializes missing filesystem metadata in `pre-start`, only
on `spec.bootstrap` when `juicefs.volume.initialize` is enabled.
`juicefs format --no-update` leaves existing filesystems unchanged; backend errors
fail startup.

The CF runtime-config adds a full-root mount to `control` and the volume driver
to `compute`. Both consume the broker VM's shared `juicefs_mount` BOSH link for
the filesystem name and all backend settings, credentials, and trust roots.
The broker's volume mounter is the authoritative configuration source;
registration errands run in CF.

FUSE clients run directly under Monit so they can use the host `/dev/fuse` and
mount namespace. In the default WebDAV setup, clients connect directly to
`https://blobstore.service.juicefs.internal:4443/admin/`. BOSH DNS aliases resolve
that hostname to the backend group. The server certificate has a matching SAN,
and clients trust the separate CA published through the mounter's shared link. There is no
local WebDAV proxy.

When upgrading a filesystem previously formatted with the loopback endpoint,
first deploy DNS, the SAN certificate, and client CA trust. Update its stored
bucket URL using `juicefs config --bucket` with the direct HTTPS endpoint and
the linked CA in `SSL_CERT_FILE` before removing the old proxies. This changes
the endpoint, not the filesystem UUID or stored objects.

## Prerequisites

- [BOSH CLI v2](https://bosh.io/docs/cli-v2/) and a reachable director with a config
  server for manifest variables.
- [Devbox](https://www.jetify.com/devbox) for the local Go, Git, and OpenSSL tools.
- A director with an `ubuntu-noble` stemcell and cloud-config containing `z1`,
  network `default`, and VM type `default`.
- An existing CF deployment named `cf`. The supplied configuration targets the
  lab's `control` and `compute` groups; adapt the runtime-config and ops file to
  your topology before using another deployment.
- Local release blobs and vendored-package build indexes, as described below.
- A CF CLI authenticated to the target organization and space for app testing.

## Build setup

```sh
git clone --recurse-submodules https://github.com/rkoster/juicefs-release.git
cd juicefs-release
devbox install
devbox run vendor-go
```

If you cloned without submodules, run `git submodule update --init --recursive`.
The pinned upstream source is in `src/juicefs`. `vendor-go` stages its module
dependencies in `src/juicefs-deps` for offline compilation on BOSH VMs.
Run it again after changing the upstream revision.

This POC uses a **local release blobstore** (`config/final.yml`). A Git clone
alone does not contain the artifacts needed to build: restore `blobs/`,
`final_blobs/`, and `.final_builds/` from the development environment, or
re-vendor the packages from their source releases with `bosh vendor-package`.
The tracked package locks are:

- `packages/golang-1.25-linux/spec.lock`
- `packages/nginx_webdav/spec.lock` — vendored from CAPI release
- `packages/cf-cli-8-linux/spec.lock`

Preserve these ignored artifacts when rebuilding. A shared permanent blobstore
is not configured yet.

Create a local, ignored `bosh.env` containing the director environment and
credentials. The development environment uses a copy from
`../instant-bosh/bosh.env`. Keep credentials out of Git.

## Deploy and register

From the release root:

```sh
devbox run deploy
```

This command:

1. Sources `bosh.env` and creates and uploads a development release.
2. Updates the named `juicefs` runtime-config with the built release's explicit
   version.
3. Deploys `manifests/juicefs.yml`.
4. Redeploys CF from its director manifest, applying
   `operations/enable-juicefs-cf-volume-service.yml`.

The ops file enables CAPI volume services and adds registration/deregistration
errands. Registration is a separate step, also needed after catalog changes:

```sh
source bosh.env
bosh -n -d cf run-errand register-juicefs-broker
cf service-brokers
cf marketplace -e juicefs
```

The registration errand creates or updates the broker and enables access to its
plans. It trusts both Cloud Controller's linked CA and the configured router CA
used by the public authentication endpoint. The supplied ops file uses CF's
existing `cf_admin_password` variable.

## Use a volume

```sh
cf create-service juicefs shared foobar
cf bind-service cf-env foobar -c '{"mount":"/data"}'
cf restart cf-env
cf ssh cf-env -c 'echo hello > /data/hello.txt; cat /data/hello.txt'
```

The optional `mount` binding parameter must be a clean, absolute, non-root path.
Without it, the container path is `/var/vcap/data/<service-instance-guid>`.
Changing the container path requires unbinding and binding again, then
restarting the app. It does not change the underlying service directory.

Bindings are synchronous and stateless. The broker validates the instance and
returns mount instructions; Cloud Controller stores them. The volume driver
mounts only `/cf-volumes/<service-instance-guid>` at container startup and
reference-counts shared mounts on each cell. Binding retrieval is disabled
(`bindings_retrievable: false`).

New service directories are owned by the configurable
`juicefs.broker.directory_uid` and `juicefs.broker.directory_gid`, both defaulting
to `2000`, with mode `0770`. These IDs must match the cell's host-mapped app user.
Existing directory ownership is not changed by repeated provisioning.

### Delete a volume

```sh
cf unbind-service cf-env foobar
cf restart cf-env
cf delete-service foobar -f --wait
```

**Deprovisioning deletes the instance's files.** The broker uses JuiceFS `rmr
--skip-trash`, removes its instance record, and treats an already absent
directory as successful cleanup. Unbinding alone does not delete files.

## Plans and quotas

Set `juicefs.broker.plans` on the `juicefs-broker` job in the deployment manifest:

```yaml
juicefs:
  broker:
    plans:
    - id: 40bf3e21-885a-4c2a-99b4-d4e8186c467e
      name: shared
      description: Shared JuiceFS directory without a quota
      free: true
      capacity_gib: 0
      inodes: 0
    - id: 5d3c1f42-eaa0-4d1b-88ce-b26cf6545cb1
      name: small
      description: Shared JuiceFS directory with 1 GiB and 1000 inodes
      free: true
      capacity_gib: 1
      inodes: 1000
```

Names, descriptions, pricing flags, and limits are configurable through BOSH.
Keep plan IDs stable; the `shared` ID above is the original plan ID. Zero limits
mean unlimited. To use a limited plan:

```sh
cf create-service juicefs small my-volume
```

The broker creates the directory and installs the selected quota before
returning provisioning success. Limits are enforced by JuiceFS across mounts
of the instance, rather than by each app or volume driver.

The chosen plan and limits are recorded under
`/.juicefs-broker/instances/<guid>.json`, outside the app-mounted subtree.
Pending records allow quota application to be retried after failure; completed
retries do not reset quotas. Pre-quota instances retain the unlimited shared
plan. Broker VM replacement does not lose these records.

Plan configuration changes affect **new instances only**. Plan upgrades and
downgrades are not supported. Run `devbox run deploy` and the registration
errand after changing the catalog.

Quotas limit directory usage; they do not reserve backend disk space. Physical
object consumption can differ because of compression, deleted objects, and
other filesystem overhead. Inode enforcement can lag accounting, so the limit
is not an exact per-create admission count.

## Backend configuration

Configure backends only on the `juicefs-volume` job in `juicefs-broker`:

```yaml
juicefs:
  metadata:
    type: mysql
    host: mysql.example.internal
    port: 3306
    database: juicefs
    user: juicefs
    password: ((mysql_password))
    options: parseTime=true
  storage:
    type: s3
    bucket: https://mybucket.s3.us-east-2.amazonaws.com
    access_key: ((s3_access_key))
    secret_key: ((s3_secret_key))
    session_token: ""
    ca_cert: ""
```

Metadata types are `postgres` and `mysql`. `options` is an optional query string
for the upstream database driver; ports default to 5432 and 3306 respectively.
Storage types are `webdav` and `s3`; `bucket` is the upstream JuiceFS bucket URL.
S3-compatible services can use their appropriate HTTP(S) bucket URL. Optional
`session_token` supports temporary credentials, and `ca_cert` supplies a custom
PEM trust root; an empty CA uses system trust. Secrets are passed to the CLI
through environment variables rather than command arguments.

These complete property objects are published by the mounter's `juicefs_mount`
link. CF's mount and driver consume that link directly from deployment
`juicefs`; they have no independent backend overrides. Broker quota operations
reuse the mounter's rendered configuration, including its metadata engine.

`operations/use-mysql-s3.yml` provides an example deployment ops file with
external backend variables. PostgreSQL/WebDAV remains the default. Provision
the external database, object bucket, and access credentials before initializing
a new filesystem. The bundled backend jobs are not removed by that example ops
file. Changing properties is not a data migration: `format --no-update` does
not rewrite an existing filesystem's storage settings or copy metadata/objects.

The linked PostgreSQL/WebDAV configuration is validated on the live POC;
MySQL/S3 support has configuration tests but has not been integration-tested.

## Verification and troubleshooting

Check deployed processes and service status:

```sh
source bosh.env
bosh -d juicefs instances --ps
bosh -d cf instances --ps
cf service foobar
cf app cf-env
cf logs cf-env --recent
```

For deployment failures, start with `bosh task <task-id> --debug`. For app
`failed to mount volume` errors, inspect the cell's Rep logs and
`/var/vcap/sys/log/juicefs-volume-driver/`. Mount and initialization logs are
under `/var/vcap/sys/log/juicefs-volume/`; broker logs are under
`/var/vcap/sys/log/juicefs-broker/`.

Focused broker tests can run without building the upstream JuiceFS binary:

```sh
devbox run -- go test -race ./src/cf-volume-broker/main.go ./src/cf-volume-broker/main_test.go
devbox run -- go test -race ./src/cf-volume-driver/main.go ./src/cf-volume-driver/main_test.go
devbox run -- go test ./src/broker-registration/main.go ./src/broker-registration/main_test.go
devbox run -- go test ./src/juicefs-service/main.go ./src/juicefs-service/main_test.go
```

The deployed POC has verified provision/bind, container read/write, persistence
across app restarts, custom container paths, and capacity/inode quota rejection.
Temporary quota-service deletion was verified to remove the directory, quota,
and instance metadata.

## Current scope

- Singleton PostgreSQL and WebDAV backends; no HA or backup automation. Keep the
  blobstore at one instance: separate persistent disks are not shared storage.
- Shared backend credentials and subdirectory mounts provide a restricted view,
  not strong isolation against users with access to those credentials.
- The broker endpoint currently uses HTTP with basic authentication on the
  deployment network; WebDAV uses TLS.
- Registration currently uses CF admin credentials. A dedicated account should
  be used outside the lab.
- Local release artifacts and lab-specific CF selectors require setup before
  deploying from a fresh clone.

See [AGENTS.md](AGENTS.md) for development notes and environment-specific
findings.
