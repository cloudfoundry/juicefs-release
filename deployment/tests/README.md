# Deployment configuration tests

This Go test package checks the CF ops/runtime-config targets with
`bosh interpolate`. Fixtures represent the standard upstream group names and
instant-bosh's consolidated names. The tests verify that the default config
targets `api`, `cc-worker`, `scheduler`, and `diego-cell`, while the explicit
instant-bosh compatibility files target `control` and `compute`.

Run the fixture-backed tests from the repository root with Go 1.25 and BOSH CLI installed:

```sh
scripts/test-deployment
```

The tests also verify the validation-backend DNS aliases, required acceptance
errand properties, and VM types (`minimal` for standard cf-deployment, `default`
for instant-bosh). The fixture files contain the CF and instant-bosh properties
this release depends on and are the test inputs of record.
