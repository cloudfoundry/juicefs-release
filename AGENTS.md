# Development loop

This is the `juicefs` BOSH release. `src/juicefs` is a pinned upstream Git submodule;
`packages/juicefs` builds it with the vendored `golang-1.25-linux` BOSH package.
`nginx_webdav` is vendored from CAPI release and powers our own singleton
`blobstore` job; re-vendor it from `../capi-release` when upgrading CAPI's package.
The vendored package locks live in `packages/nginx_webdav/spec.lock` and
`packages/golang-1.25-linux/spec.lock`. Their build indexes in `.final_builds/`
and package blobs in `final_blobs/` are local and ignored by Git.
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
then runs `bosh -d juicefs deploy manifests/juicefs.yml`. The manifest uses
`version: latest` for the `juicefs` release. The director must have an
`ubuntu-noble` stemcell and cloud-config with `z1`, `default` network and
`default` VM type. Postgres and BPM are pinned external releases. The singleton
WebDAV blobstore keeps objects on its persistent disk at
`/var/vcap/store/juicefs-blobstore/`. The job provides a `webdav` BOSH link;
future JuiceFS clients should consume that link rather than hardcoding an address.
The authenticated WebDAV endpoint is `https://<link address>:<blobstore.port>/admin/`;
the `blobstore.admin_users` and `blobstore.tls.cert` link properties provide its
credentials and certificate. Keep `blobstore` at one instance: each instance's
persistent disk holds separate objects, so scaling it would not create shared storage.

After deploying, run `bosh -d juicefs instances --ps`, then
`bosh -d juicefs run-errand verify-backends --keep-alive` to verify both links,
Postgres access and WebDAV `PROPFIND`, `PUT`, `GET`, and `MKCOL` against our
blobstore. To initialize the `juicefs-poc` volume once, run
`bosh -d juicefs run-errand format-volume --keep-alive`. This is idempotent:
`juicefs format --no-update` returns without modifying an existing format.
JuiceFS metadata lives in Postgres, data blocks live in the WebDAV blobstore,
and each mount has its own ephemeral local cache. All mounts of this volume
must use those same backends. A loopback reverse proxy on each mount VM uses
the blobstore link's IP, credentials and pinned TLS certificate; the volume's
stored endpoint is loopback so each client must run the proxy.

`juicefs-volume` is a BPM-managed, foreground FUSE client in the release. It
is declared with zero instances in this POC manifest while the instant-bosh
stemcell/BPM device access is unresolved. Scaling it to two instances was tested:
JuiceFS reached Postgres and loaded the format, but mounting failed because
the BPM container could not open `/dev/fuse` (`operation not permitted`), even
with `unsafe.privileged: true` and a device node created inside BPM's `/dev`.
The host VM can open `/dev/fuse`; the failure is inside BPM. JuiceFS then tried
the fallback `/bin/fusermount`, which is absent from this stemcell. Do not
enable the mount instances until FUSE access and host-visible mount propagation
have been verified on the target stemcell.
The certificate variable uses `update_mode: converge` so a config server can
regenerate it when generation options change. This option is not honored when
using a local `--vars-store` with `bosh create-env`.

The instant-bosh test director returns link DNS addresses that are not resolvable
on its VMs, so the errand requests IP addresses from both links. Its config server
currently issues a legacy CN-only blobstore certificate even with SAN options;
the errand verifies the certificate chain and pins the exact certificate provided
by the blobstore link. The earlier CAPI blobstore job returned HTTP 405 for
`MKCOL`; that is why this release provides its own WebDAV job using CAPI's
vendored `nginx_webdav` package.
On the deployed POC, `verify-backends` exits 0: Postgres `SELECT 1`, WebDAV
`PROPFIND`, `PUT`, `GET`, `MKCOL`, and cleanup `DELETE` all succeed.
