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

CI disables all runner swap for the current memory-pressure experiment. It
samples host memory, swap activity, CPU/I/O/memory pressure and container usage
throughout the workflow in `resource-timeline.log`. Diagnostics also save kernel
messages in `host-kernel.log` and container state (including `OOMKilled`) in
`docker-container-state.log`.

On a spec failure, the errand writes the build state/error, web-process stats,
and recent application/staging logs from CF Log Cache to `GinkgoWriter` before
deleting the application. These diagnostics appear in the failed spec's output;
collection is best-effort and uses a separate timeout so cleanup still runs if
the spec context has expired or Log Cache is unavailable.
