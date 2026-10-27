# Acceptance testing

The [acceptance workflow](../.github/workflows/acceptance.yml) runs a disposable
Cloud Foundry environment to check that the JuiceFS volume service works end to
end.

It provisions and binds a volume to an app, writes a random value to a file,
restarts the app, and verifies the value is still available. This exercises the
service broker, volume driver, and persistence across app restarts.

The workflow can also be run locally from a disposable environment. Its commands
are in [`scripts/acceptance/run`](../scripts/acceptance/run).

On failure, download the acceptance artifact from the GitHub Actions run summary
for test results and diagnostic logs.
