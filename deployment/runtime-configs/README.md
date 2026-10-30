# Runtime configs

## JuiceFS volume driver

[`juicefs-volume-driver.yml`](juicefs-volume-driver.yml) is the default runtime-config
for upstream `cf-deployment`. It installs the JuiceFS volume driver on Diego
cells by adding the `juicefs-volume-driver` job to the `diego-cell` instance
group in deployment `cf`, enabling cells to mount
the service-instance directories provisioned by the JuiceFS broker. Adjust the
addon's `include` filters if your CF deployment or cell group has another name.

The driver consumes the shared `juicefs_mount` link from deployment `juicefs`.
That link supplies the filesystem name, backend configuration, credentials, and
trust roots; the cell driver does not need independent backend settings. Deploy
JuiceFS first so this link is available.

Apply the runtime-config with the explicit uploaded JuiceFS release version:

```sh
bosh update-config --type=runtime --name=juicefs -n \
  deployment/runtime-configs/juicefs-volume-driver.yml \
  -v juicefs_release_version="$release_version"
bosh deploy -n -d cf <(bosh manifest -d cf)
```

Updating the runtime-config records the configuration on the director;
redeploying CF installs or updates the addon jobs on the cells.

### Azure managed identities

Apply [`operations/use-azure-managed-identities.yml`](operations/use-azure-managed-identities.yml)
to the runtime-config to colocate the Azure PostgreSQL authentication proxy on
each targeted cell and connect the driver's local proxy link. BPM must already
be present on those cells. See [Azure managed identities](../../docs/azure-managed-identities.md)
for backend configuration, required variables, and deployment commands.

## BOSH DNS prerequisite

Install BOSH DNS through the upstream
[`bosh-deployment/runtime-configs/dns.yml`](https://github.com/cloudfoundry/bosh-deployment/blob/master/runtime-configs/dns.yml).
This is separate from the JuiceFS volume-driver runtime-config.

For instant-bosh development environments, DNS is currently embedded only in CF.
Apply [`../operations/misc/ibosh-bosh-dns-fix.yml`](../operations/misc/ibosh-bosh-dns-fix.yml)
to JuiceFS to reuse CF's global DNS certificates. Omit this workaround when the
director already supplies DNS through a runtime-config.

## Standard cf-deployment and instant-bosh

The default runtime-config and CF operations target upstream `cf-deployment`
groups. For instant-bosh's consolidated manifest, use the matching files under
`instant-bosh/`; its `api`, `cc-worker`, and `scheduler` groups are combined as
`control`, while `diego-cell` becomes `compute`. Acceptance and Devbox scripts
select the instant-bosh variants.

Run `scripts/test-deployment` from the repository root to check deployment
operations and runtime-config targets against the checked-in standard CF and
instant-bosh fixtures.
