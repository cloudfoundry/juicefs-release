# Deployment configuration tests

This Go test package checks the CF ops/runtime-config targets with
`bosh interpolate`. Fixtures represent the standard upstream group names and
instant-bosh's consolidated names. The tests verify that the default config
targets `api`, `cc-worker`, `scheduler`, and `diego-cell`, while the explicit
instant-bosh compatibility files target `control` and `compute`.

Run the tests from the repository root with Go 1.25 and BOSH CLI installed:

```sh
scripts/test-deployment
```

The tests also verify the validation-backend DNS aliases and the acceptance
errand VM type (`minimal` for standard cf-deployment, `default` for instant-bosh).
The script automatically validates the standard CF ops against the upstream
manifest embedded in the instant-bosh checkout, when that checkout is present.
To use another cf-deployment manifest, set `CF_DEPLOYMENT_MANIFEST`:

```sh
CF_DEPLOYMENT_MANIFEST=/path/to/cf-deployment.yml scripts/test-deployment
```
