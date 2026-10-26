# Azure managed identities

This release can use VM managed identities for both Azure PostgreSQL metadata
and Azure Blob data blocks, without database passwords, storage account keys,
or SAS tokens. Azure integration must be validated in your environment before
relying on it; the default lab deployment uses PostgreSQL/WebDAV.

## Architecture

- On the broker VM and every Diego cell, `azure-postgres-auth-proxy` listens on
  `127.0.0.1:5432`. JuiceFS speaks PostgreSQL to that local endpoint. The proxy
  obtains an Entra token using the VM identity and connects to Azure PostgreSQL
  over TLS with CA and hostname verification.
- JuiceFS connects directly to Azure Blob over verified HTTPS. Its Azure SDK
  obtains and renews tokens using the VM identity; no Blob proxy is needed.
- Both paths explicitly select `ManagedIdentityCredential`; there is no fallback
  to service-principal secrets or development CLI credentials in this mode.
- PostgreSQL and Blob can use different user-assigned identity client IDs. An
  empty client ID selects the VM's system-assigned identity.

The local PostgreSQL listener has **no client authentication or TLS**. Any process
able to reach host loopback can attempt access using the proxy's identity. Keep
the listener inaccessible from app containers. VM identities also require
appropriate restrictions on app access to the Azure IMDS endpoint
(`169.254.169.254`); loopback binding alone does not control token acquisition.
Unix-domain sockets are a future enhancement.

## 1. Provision Azure resources and identities

Provision an Azure Database for PostgreSQL Flexible Server with Microsoft Entra
authentication enabled, a database, and an Azure Blob storage account/container.
Configure private networking and DNS as required. Use the ordinary service DNS
names (including through private DNS), not private endpoint IPs, so TLS hostname
verification succeeds. Every broker/cell VM needs access to IMDS and both data
endpoints; the identity/token service must also be reachable as required by Azure.

Attach the selected identities to **every broker and compute VM**, including
replacement and scale-out VMs. Configure this through your Azure CPI-supported
VM/cloud-config mechanism or infrastructure automation; the BOSH job does not
assign identities. Merely supplying a client ID does not attach an identity.

A user-assigned identity is convenient because replacement VMs retain the same
database principal and RBAC assignment. With separate system-assigned identities,
provision database grants and Blob RBAC for every VM identity, and repeat when
the VM identity changes. All mounts use the same configured PostgreSQL username;
an Entra group mapped to that role can accommodate multiple VM identities where
supported by your Azure PostgreSQL setup.

### PostgreSQL authorization

As the server's Entra administrator, create/map the managed-identity principal
using Azure PostgreSQL's `pgaadauth_create_principal_with_oid` function. For example,
for one user-assigned identity, substitute its **principal/object ID**, not its
client ID:

```sql
SELECT * FROM pgaadauth_create_principal_with_oid(
  'juicefs-identity', '<identity-principal-object-id>', 'service', false, false
);
CREATE DATABASE juicefs OWNER "juicefs-identity";
```

Follow Azure's instructions for the database in which principal-management
functions are available. The configured username must match the mapped role.
For an existing database, grant CONNECT, schema USAGE/CREATE, and permissions on
existing JuiceFS tables/sequences; JuiceFS needs to create and update its schema.
Do not give the application identity Entra administrator privileges.

### Blob authorization

Assign **Storage Blob Data Contributor** to each selected identity at the
container scope (or a narrower custom role covering all required operations).
Pre-create the container so the application need not provision storage resources.
Example using an already authenticated Azure CLI:

```sh
az role assignment create \
  --assignee-object-id '<identity-principal-object-id>' \
  --assignee-principal-type ServicePrincipal \
  --role 'Storage Blob Data Contributor' \
  --scope '<storage-account-resource-id>/blobServices/default/containers/<container>'
```

Allow time for RBAC propagation. Once existing consumers have been migrated,
disable storage account Shared Key access if required by your policy.

## 2. Configure the release

Create local `azure-vars.yml` (ignored by Git):

```yaml
azure_postgres_host: myserver.postgres.database.azure.com
azure_postgres_database: juicefs
azure_postgres_user: juicefs-identity
azure_postgres_managed_identity_client_id: "" # Or the user-assigned client ID
azure_postgres_ca_cert: "" # Empty uses packaged public CA roots
azure_container_url: https://mycontainer.blob.core.windows.net
azure_account_name: mystorageaccount
azure_blob_managed_identity_client_id: "" # Or the user-assigned client ID
azure_blob_ca_cert: ""
```

The JuiceFS `wasb` bucket hostname starts with the **container**, not the account.
`access_key` still contains the non-secret account name. Sovereign-cloud Blob
domains can be configured, but the PostgreSQL proxy's token audience is currently
hardcoded for public Azure; sovereign-cloud PostgreSQL requires upstream changes.

`deployment/operations/use-azure-managed-identities.yml` adds BPM and the proxy to the
broker group and replaces the mounter's backend settings. The matching
`deployment/runtime-configs/operations/use-azure-managed-identities.yml` adds a proxy to each
targeted CF compute VM. **BPM must already be colocated on compute** (as in
cf-deployment); add it in your CF manifest if absent. The proxy shares the host
network, as required for loopback clients, DNS, and IMDS.

Both clients consume a local `azure_postgres_proxy` link and validate colocation;
the proxy link must not be shared across deployments. The existing shared
`juicefs_mount` link remains authoritative for database/user, storage, and volume.
Broker quota operations use the mounter's rendered local configuration.

The storage fields are:

| Field | Meaning |
| --- | --- |
| `managed_identity` | Boolean, defaults to false; true restricts authentication to VM managed identity |
| `managed_identity_client_id` | Optional user-assigned client ID; empty selects system-assigned |
| `access_key` | Required storage account name |
| `secret_key`, `session_token` | Must be empty with explicit managed identity |
| `ca_cert` | Optional replacement PEM trust bundle; empty uses packaged public roots |

An inherited `AZURE_STORAGE_CONNECTION_STRING` is rejected in managed-identity
mode. A client ID without `managed_identity: true` is rejected. Managed identity
requires `wasb` and an HTTPS bucket. Custom CA bundles must include all roots
needed by that process. Public trust is packaged from pinned certifi sources,
not loaded from stemcell certificate files.

For Blob only, apply `deployment/operations/use-azure-blob-managed-identity.yml` instead;
no proxy or runtime-config modification is needed. For PostgreSQL only, colocate
the proxy/BPM and consume its link while retaining your existing storage object.
The proxy's properties live under `azure_postgres_auth_proxy`: `host`,
`upstream_port`, local `port`, `client_id`, `ca_cert`, `connect_timeout`, and
`shutdown_timeout`. The listener always binds to `127.0.0.1`.

## 3. Build and deploy

Initialize submodules before building. Pre-packaging hooks vendor Go dependencies
in BOSH's temporary source copy using the Devbox Go toolchain:

```sh
git submodule update --init --recursive
source bosh.env
devbox run -- bosh create-release --force
bosh upload-release
```

Set `release_version` to the explicit version reported by `create-release`, then:

```sh
bosh update-config --type=runtime --name=juicefs -n \
  deployment/runtime-configs/juicefs-volume-driver.yml \
  -o deployment/runtime-configs/operations/use-azure-managed-identities.yml \
  --vars-file azure-vars.yml -v juicefs_release_version="$release_version"
bosh -n -d cf deploy <(bosh -d cf manifest) \
  -o deployment/operations/cf/share-cloud-controller.yml
bosh -n -d juicefs deploy deployment/juicefs.yml \
  -o deployment/operations/misc/ibosh-bosh-dns-fix.yml \
  -o deployment/operations/misc/ibosh-cf-credentials.yml \
  -o deployment/operations/use-azure-managed-identities.yml --vars-file azure-vars.yml
bosh deploy -n -d cf <(bosh manifest -d cf) \
  -o deployment/operations/cf/enable-volume-services.yml
```

Use these Azure-specific commands on subsequent deployments too. The default
`devbox run deploy` applies the lab backend settings and runtime-config and is
not the Azure deployment entry point. The bundled PostgreSQL/WebDAV backend
group is retained by the example operations; remove unused jobs/resources in
your deployment-specific manifest after validating the external backends.

Bootstrap initialization now runs in the supervised mount launcher, rather than
`pre-start`, so Monit can start the colocated proxy concurrently. It retries
`format --no-update` on `spec.bootstrap` and fails if initialization cannot
complete. Existing filesystems are never reconfigured by this step.

Changing properties does not migrate metadata, objects, or stored filesystem
credentials. For an existing key/SAS-authenticated Azure volume, explicitly update
its stored storage credentials using `juicefs config` following upstream guidance,
then validate every mount before disabling the old credentials. Changing from
WebDAV or local PostgreSQL requires a separate data/metadata migration.

## 4. Validate

- Check `bosh -d juicefs instances --ps` and `bosh -d cf instances --ps` for
  healthy proxy, mount, broker, and cell-driver processes.
- Inspect `/var/vcap/sys/log/azure-postgres-auth-proxy/` (BPM stdout/stderr),
  `/var/vcap/sys/log/juicefs-volume/initialize.stderr.log`, and cell-driver logs.
- Register the broker, provision/bind a volume, and verify app read/write,
  persistence across restarts, quota application, unbinding, and cleanup.
- Verify traffic uses the private endpoints, and that app containers cannot
  reach the proxy or IMDS. Test rejection of an incorrect/untrusted database
  certificate and denied database/Blob permissions.
- Keep a mount active beyond token expiry and force new backend connections to
  exercise refresh. Test proxy restart and VM replacement with identity attachment.
- Exercise Blob listing, deletion, range reads, and any copy/tiering operations
  you use; validating credential acquisition alone is insufficient.

The proxy currently does not implement PostgreSQL cancellation messages or
client-side TLS negotiation; local clients must use `sslmode=disable`, as the
templates enforce. Database authorization is performed by Azure, while socket
access on the VM is not authenticated.

### Local implementation checks

From the release root, run the wrapper and template tests (Ruby requires the
standard `minitest` library):

```sh
devbox run -- scripts/test-go
ruby tests/render_templates.rb
```

Run the TLS configuration regression test inside the proxy's module so it uses
the exact pgx version shipped with the proxy:

```sh
(cd src/azure-postgres-auth-proxy && go test -mod=mod ../../tests/azure-proxy-tls_test.go)
```

This verifies that `PGSSLROOTCERT=system` promotes the upstream proxy's explicit
`sslmode=require` to full chain/hostname verification without a plaintext
fallback. Azure resource access, IMDS reachability under BPM, and live token
refresh still require the integration checks above.
