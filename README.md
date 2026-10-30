# JuiceFS BOSH release

> **Disclaimer — pre-GA:** This repository is part of the Cloud Foundry
> [Agentic Workloads Working Group](https://github.com/cloudfoundry/community/blob/main/toc/working-groups/agentic-runtime.md).
> Its intended use is to provide persistent storage for agents running on top of
> Cloud Foundry. The release is not yet generally available (GA), and breaking
> changes may still occur. For the agent workloads targeted here, adapting to
> these changes should be manageable in most cases; plan for that flexibility
> when adopting the release.

A proof-of-concept [JuiceFS](https://juicefs.com/) volume service for Cloud Foundry.
The release includes a service broker, a Diego volume driver, and a shared
filesystem with PostgreSQL or MySQL metadata and WebDAV, S3, or Azure Blob object storage.
The base deployment accepts external metadata and object-storage configuration.
The `deployment/operations/non-ha-validation.yml` ops file adds singleton
PostgreSQL and persistent WebDAV backends for validation.

Applications can provision a directory, bind it at a chosen container path, and
keep files across container restarts. BOSH-configurable service plans apply
directory capacity and inode quotas.

## Acceptance testing

The [CF volume acceptance workflow](.github/workflows/acceptance.yml) starts a
disposable instant-bosh director and Cloud Foundry on a standard GitHub-hosted Ubuntu runner.
Its BOSH errand provisions and binds a JuiceFS service, writes a random GUID
through an app endpoint, restarts the app, and confirms the GUID persists.
See [acceptance testing](docs/acceptance-testing.md) for runner setup, local
execution, and reports.

## Architecture

```text
Cloud Foundry CLI → Cloud Controller → JuiceFS service broker
                                           │
                                           └─ provisions /cf-volumes/<instance-guid>

Diego cell → JuiceFS volume driver → subdirectory FUSE mount → app container
                      │
                      ├─ PostgreSQL / MySQL: shared filesystem metadata
                      └─ WebDAV / S3 / Azure Blob: persistent data blocks
```

With the non-HA validation ops file, `juicefs` has two singleton instance groups:

| Group | Jobs | Storage |
| --- | --- | --- |
| `juicefs-backends` | PostgreSQL, BPM, WebDAV blobstore | Persistent disk for metadata and objects |
| `juicefs-broker` | Full-root JuiceFS mount, service broker | Ephemeral cache; instance records live on JuiceFS |

The backend group updates first, using BOSH's default serial instance-group
updates. The mount initializes missing filesystem metadata in its supervised launcher, only
on `spec.bootstrap` when `juicefs.volume.initialize` is enabled.
`juicefs format --no-update` leaves existing filesystems unchanged; backend errors
fail startup.

The default CF runtime-config adds the volume driver to upstream cf-deployment's
`diego-cell` group; an instant-bosh compatibility variant targets its consolidated
`compute` group. The driver consumes the broker VM's shared `juicefs_mount` BOSH
link for the filesystem name and all backend settings, credentials, and trust roots.
The broker's volume mounter is the authoritative configuration source;
registration errands are colocated on `juicefs-broker` in the JuiceFS deployment.

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
- Go 1.25, Git, and OpenSSL to build the release locally. They must be available
  on `PATH`; Devbox is an optional convenience for providing these tools, not a
  project requirement.
- A director with an `ubuntu-noble` stemcell and cloud-config containing `z1`,
  network `default`, and VM type `default`.
- An existing CF deployment named `cf`. Default CF ops/runtime-configs target
  upstream cf-deployment groups. Instant-bosh's consolidated group names are
  isolated in `deployment/operations/cf/instant-bosh/` and
  `deployment/runtime-configs/instant-bosh/`.
- Local release blobs and vendored-package build indexes, as described below.
- A CF CLI authenticated to the target organization and space for app testing.

## Build setup

```sh
git clone --recurse-submodules https://github.com/cloudfoundry/juicefs-release.git
cd juicefs-release
```

If you cloned without submodules, run `git submodule update --init --recursive`.
The pinned upstream source is in `src/juicefs`. Each upstream submodule package's `pre_packaging`
hook runs `go mod vendor` against BOSH's temporary source copy before archiving
the package. Release creation requires Go 1.25, Git, OpenSSL, and the BOSH CLI.
Install those prerequisites directly, or use [Devbox](https://www.jetify.com/devbox)
to enter a shell with the repository's declared Go, Git, and OpenSSL versions:

```sh
devbox install
devbox run -- bosh create-release --force
```

When the prerequisites are already on `PATH`, run `bosh create-release --force`
directly. `devbox run deploy` is likewise a convenient wrapper around
`scripts/deploy`; the deployment workflow itself does not require Devbox.

Release creation needs access to the pinned Go modules (or a populated local
module cache). Compilation VMs receive the generated vendor trees and compile
with module downloads disabled. Upstream dependencies are not stored in Git
or written into the source submodules. Release-owned modules under `src/` check
in their own `vendor/` directories, including their test/generation dependencies.
The proxy is pinned to v0.1.5; `src/certifi` supplies pinned public CA roots.

The upstream client is the only binary in `packages/juicefs`. Release-owned
tools build independently in `juicefs-launcher`, `cf-volume-broker`,
`cf-volume-driver`, `broker-registration`, and `verify-backends` packages, each
with its own Go module. Registration and deregistration share one
`broker-registration` binary. Jobs declare the client and helper packages they
actually invoke, so helper changes do not rebuild the upstream client.

Release blobs are stored in **`gs://juicefs-release-blobs`** (`config/final.yml`).
The vendored-package build indexes in `.final_builds/` and package locks are
tracked so a fresh clone can download the pinned package blobs. The locks are:

- `packages/golang-1.25-linux/spec.lock`
- `packages/nginx_webdav/spec.lock` — vendored from CAPI release

Run `bosh sync-blobs` to populate the local blob cache. Uploaders configure GCS
credentials in ignored `config/private.yml`, then run `bosh upload-blobs` after
adding blobs. Never commit that file or its service-account key. Preserve local
`blobs/` and legacy `final_blobs/` caches when rebuilding.

Create a local, ignored `bosh.env` containing the director environment and
credentials. The development environment uses a copy from
`../instant-bosh/bosh.env`. Keep credentials out of Git.

## Deploy and register

From the release root, with the prerequisites above available:

```sh
./scripts/deploy
```

Alternatively, run `devbox run deploy` to use the same script inside the
optional Devbox environment. `scripts/deploy` is the source of truth for the
deployment sequence:

It reads credentials from `bosh.env`, builds and uploads the release, deploys
JuiceFS with the non-HA validation configuration, updates the JuiceFS runtime
config, and redeploys CF with the volume-service configuration. See the
deployment and operations files for environment-specific settings.

The main JuiceFS manifest colocates `register-broker` and `deregister-broker`
as errands on the broker VM. Registration is a separate step, also needed after
catalog changes:

```sh
source bosh.env
bosh -n -d juicefs run-errand register-broker --instance=juicefs-broker/0
cf service-brokers
cf marketplace -e juicefs
```

The base `deployment/juicefs.yml` requires `juicefs_metadata` and
`juicefs_storage` objects in a vars file, or an external-backend ops file.
It also requires `juicefs_cf_password`, `juicefs_cf_system_domain`, and
`juicefs_cf_ca_certificates` for registration, unless using the instant-bosh
credential ops. Registration calls the public `https://api.<system_domain>`
route without a cross-deployment Cloud Controller link. Use
`deployment/operations/misc/provide-cf-api-ca.yml` to configure the trusted API CA.
Supply `juicefs_cf_api_ca_certificate` from a vars file or `-v` when applying it.
For singleton validation, apply `deployment/operations/non-ha-validation.yml`
instead; it supplies both objects, Postgres/WebDAV jobs, credentials, certificates,
and backend DNS aliases. Apply its CF companion
`deployment/operations/cf/non-ha-validation.yml` to CF.
For instant-bosh, the misc DNS workaround adds a JuiceFS addon that reuses
CF's global certificates. For other directors, use the upstream DNS
runtime-config; see [DNS setup](deployment/runtime-configs/README.md).

The registration errand creates or updates the broker and enables access to its
plans. It trusts both Cloud Controller's linked CA and the configured router CA
used by the public authentication endpoint. The supplied ops file uses CF's
existing `cf_admin_password` variable.

Both registration errands use the vendored `cloudfoundry/go-cfclient/v3` library
directly; no CF CLI is packaged or executed. Credentials and tokens remain in
memory, and no `CF_HOME` state is written. Registration waits for asynchronous
broker jobs, then enables every JuiceFS plan using the plan visibility API.
Catalog discovery is scoped to the configured broker GUID. Deregistration deletes
unused plans and offerings before deleting the broker; an in-use plan causes an
error rather than purging service instances or bypassing backend deprovisioning.

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
Recursive deletion uses `juicefs.broker.deletion_threads` (default 4), rather
than the CLI's 50-worker default, to limit metadata database connection pressure.
The broker verifies the directory is absent before removing its instance record.

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
downgrades are not supported. Run `./scripts/deploy` (or `devbox run deploy`)
and the registration errand after changing the catalog.

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
Storage types are `webdav`, `s3`, and `wasb` (Azure Blob); `bucket` is the upstream JuiceFS bucket URL.
S3-compatible services can use their appropriate HTTP(S) bucket URL. Optional
`session_token` supports temporary credentials, and `ca_cert` supplies a custom
PEM trust root; an empty CA uses system trust. Secrets are passed to the CLI
through environment variables rather than command arguments.

These complete property objects are published by the mounter's `juicefs_mount`
link. CF's volume driver consumes that link directly from deployment
`juicefs`; they have no independent backend overrides. Broker quota operations
reuse the mounter's rendered configuration, including its metadata engine.

`deployment/operations/use-mysql-s3.yml` provides an example deployment ops file
with external backend variables. PostgreSQL/WebDAV is selected by the non-HA
validation ops file. Provision
the external database, object bucket, and access credentials before initializing
a new filesystem. External backend ops can be applied directly to the base
manifest without deploying singleton backend jobs. Changing properties is not a
data migration: `format --no-update` does
not rewrite an existing filesystem's storage settings or copy metadata/objects.

The linked PostgreSQL/WebDAV configuration is validated on the live POC;
MySQL/S3/Azure Blob support has configuration tests but has not been integration-tested.

### Azure Blob Storage

Use `deployment/operations/use-azure-blob.yml`, or configure the mounter's storage object:

```yaml
juicefs:
  storage:
    type: wasb
    bucket: https://mycontainer.blob.core.windows.net
    access_key: ((azure_account_name))
    secret_key: ((azure_account_key))
    session_token: ""
    ca_cert: ""
```

JuiceFS uses `wasb` for Azure Blob. Its bucket URL contains the **container name**
as the first hostname component, not the account name; `access_key` supplies the
storage account name. `secret_key` is the account key. For SAS authentication,
leave `secret_key` empty and put the SAS token in `session_token`. Without either
a key or SAS token, upstream JuiceFS attempts Azure's default credential chain
(such as VM managed identity); that identity must be available on every mount VM.
Azure sovereign-cloud endpoints can use the corresponding blob domain.
All these settings reach the driver through the same authoritative mounter link.

For explicit system-assigned or user-assigned managed identity, set
`managed_identity: true` and optional `managed_identity_client_id` on the storage
object. See [Azure managed identities](docs/azure-managed-identities.md) for VM
identity assignment, PostgreSQL proxy colocation, Blob RBAC, and deployment steps.

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

PostgreSQL metadata and WebDAV blocks share the backend persistent disk (50 GiB
in the example manifest). Monitor its free space: a full object store can also
prevent PostgreSQL checkpoints, causing metadata EIO errors across all mounts.
`deployment/operations/non-ha-validation.yml` sets the backend persistent disk
to 50 GiB; increase its `persistent_disk` value for larger workloads.
Preserve the disk and recover the database before reconnecting affected clients.
If an app retains a disconnected FUSE mount, stop it and wait for its volume
unmount to finish before starting it again; an immediate restart can race the
old container's delayed unmount.

Release-owned Go modules use Ginkgo v2/Gomega with Counterfeiter fakes for process,
mount-system, command execution, HTTP, and database boundaries. Real temporary
directories exercise filesystem persistence, while subprocess adapter tests
verify command execution and signal forwarding without FUSE or a BOSH director.

Run all suites with race detection and randomized order, and report statement
coverage excluding generated fakes:

```sh
scripts/test-go
GINKGO_SEED=8675309 scripts/test-go
scripts/generate-fakes
```

These commands require Go 1.25 on `PATH`; use `devbox run --` before a command
if you want Devbox to provide the declared tool versions.

Run the deployment configuration tests with:

```sh
scripts/test-deployment
```

When instant-bosh is checked out at `.acceptance-tools/instant-bosh`, the tests
also validate the default ops against its embedded upstream cf-deployment manifest.

Coverage profiles default to `/tmp/juicefs-release-coverage`; override with
`COVERAGE_DIR`. Tests and fake generation use the checked-in vendor trees with
network downloads disabled. When updating a release-owned module's dependencies,
run `go mod tidy` and `go mod vendor` in that module, regenerate fakes, then
commit `go.mod`, `go.sum`, `vendor/`, and `fakes/` together. Counterfeiter is pinned
in each module's Go `tool` directive. Generated fakes are not imported by
production code or linked into release binaries. Upstream submodules retain
pre-packaging vendoring instead.

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
