# Runtime configs

Normally install BOSH DNS using the upstream
[`bosh-deployment/runtime-configs/dns.yml`](https://github.com/cloudfoundry/bosh-deployment/blob/master/runtime-configs/dns.yml).

The pinned instant-bosh CLI embeds DNS into CF instead of installing it
director-wide. For that environment, apply
`deployment/operations/misc/ibosh-bosh-dns-fix.yml` to JuiceFS. It injects
the DNS addon and references the global `/dns_*` certificates already defined
by the CF deployment. It does not declare or regenerate those variables.
CI and the local Devbox deployment command apply this workaround; CF's own
DNS configuration is left in place. Omit this ops file on directors already
providing BOSH DNS through a runtime-config.

`juicefs-volume-driver.yml` is the separate named `juicefs` runtime-config.
