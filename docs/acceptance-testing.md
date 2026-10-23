# Cloud Foundry volume acceptance

`.github/workflows/acceptance.yml` runs on pull requests and manual dispatch. It
creates a disposable Docker-backed [instant-bosh](https://github.com/rkoster/instant-bosh)
director, deploys CF, builds this checkout as a BOSH release, deploys JuiceFS,
updates the CF cells through the named `juicefs` runtime-config, registers the
broker, and runs the `juicefs-acceptance-tests` BOSH errand.

## GitHub setup

The workflow currently uses the standard GitHub-hosted **`ubuntu-latest`** runner;
no runner variable is required. Initial runs will establish how far the full CF
deployment fits within its resources. Diagnostics include host memory, disk,
Docker disk usage, and container statistics to identify capacity limits.

Local runners need Linux x64, Docker Engine, passwordless sudo, the host FUSE
kernel module/device, `setfacl` (the `acl` package), and support for privileged containers and nested Garden
containers. Use an ephemeral, single-job runner: instant-bosh uses fixed resource
names and ports.

Startup grants the invoking user a Docker socket ACL: the director's pre-start
hook changes the socket's group to vcap (GID 1000), including on the host through
the bind mount. The ACL preserves runner access after this group change.

The workflow uses `pull_request` with read-only repository permissions. It does
not need CF credentials: the disposable director generates them. The release's
GCS blobstore must allow anonymous reads of vendored package blobs for fork PRs;
`bosh sync-blobs` fails if those reads are unavailable. Configure public read
access to those release artifacts separately from uploader credentials.

Tool versions are in `scripts/acceptance/versions.env`. The instant-bosh CLI is
built from a pinned commit because published `v0.3` predates `ibosh cf deploy`.
The director image is pinned by digest. Update these together and verify a full
run. CF release and compiled-stemcell versions come from that CLI's embedded
cf-deployment; the JuiceFS deployment reuses its installed DNS releases and
stemcell. CI uses a 10 GiB backend disk.

Director startup skips the default `latest` stemcell upload, whose registry
version-tag discovery can be slow. `ibosh cf deploy` uploads the exact stemcell
required by its compiled CF releases instead.

## What the errand verifies

The release-owned Go suite uses vendored go-cfclient v3 and Ginkgo/Gomega. It:

1. Resolves the `shared` service plan under the registered `juicefs` broker.
2. Creates a unique org, space, service instance, app, and route through CAPI.
3. Binds the volume at `/data`, uploads a static Go HTTP app, and stages it with
   CF's `binary_buildpack` on `cflinuxfs4` (no runtime downloads in the app).
4. POSTs a random UUID to `/file`; the app writes, syncs, and closes the file.
5. Restarts the app through CAPI and waits for `/identity` to change, proving
   requests are reaching a replacement process.
6. GETs `/file` and requires the exact original UUID.
7. Deletes resources in dependency order, awaiting asynchronous CAPI jobs even
   after a failed assertion.

The app endpoints require a per-run bearer token and disable response caching.
The mount directory must already exist; the app never creates a fallback `/data`.
TLS verification uses the Cloud Controller link CA and the CF router CA.

The errand can be enabled independently on the consolidated CF topology: apply
`operations/enable-juicefs-cf-volume-service.yml` followed by
`operations/enable-juicefs-acceptance-tests.yml`, supplying
`juicefs_release_version` and `juicefs_acceptance_apps_domain`, then run
`bosh -d cf run-errand juicefs-acceptance-tests` after registering the broker.
Its job properties allow choosing another broker name or plan.

## Local execution

Install Go 1.25, BOSH CLI, jq, and the pinned instant-bosh CLI; initialize the
release's submodules. On a disposable Linux Docker host, run from the release
root (inside Devbox if Go is not otherwise on PATH):

```sh
bash scripts/acceptance/run start
bash scripts/acceptance/run deploy-cf
bash scripts/acceptance/run build-release
bash scripts/acceptance/run deploy-service
bash scripts/acceptance/run test
bash scripts/acceptance/run diagnostics
bash scripts/acceptance/run destroy
```

The default work directory is ignored `.acceptance/`; override it with
`ACCEPTANCE_WORK_DIR`. It contains private deployment material, so only its
`artifacts/` subdirectory is uploaded by CI. Do not run against an existing
instant-bosh environment you want to retain: `destroy` removes its resources.

The workflow always attempts diagnostics and destruction after failure. Download
the `juicefs-acceptance-<run>-<attempt>` artifact for JUnit, errand logs, failed
BOSH task output, and JuiceFS job diagnostics. The acceptance VM is kept alive
until destruction so its JUnit report can be retrieved. Cancellation or a runner
loss can interrupt cleanup; ephemeral runners reclaim remaining resources.

`scripts/test-go` includes the app checks and a local TLS CAPI/UAA/router fixture
that executes the real suite. The fixture verifies broker scoping, executable
app upload, restart, and cleanup on both success and deliberate post-restart
data loss. Live acceptance is skipped unless `JUICEFS_ACCEPTANCE_CONFIG` points
to the rendered job JSON. Template checks run with `ruby tests/render_templates.rb`.
