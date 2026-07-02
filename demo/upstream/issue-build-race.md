Title: bug(devspace): first deploy races on shared build-container-localdev image, two of three builds fail

## Version

main (reproduced at a171cb1, still present at 7a199a7)

_Context: hit on first deploy of the DevSpace local-dev environment while
evaluating NICo — I work on DPU/bare-metal provisioning at Crusoe (an NVIDIA
Cloud Partner)._

## Describe the bug

The three custom image builds in `devspace.yaml` (`nico-api`,
`nico-bmc-proxy`, `machine-a-tron`) each start with the same guard:

```
docker image inspect build-container-localdev >/dev/null 2>&1 || \
  docker build --pull=false -t build-container-localdev -f dev/docker/Dockerfile.build-container-x86_64 .
```

DevSpace runs the three builds concurrently. On a host where
`build-container-localdev` does not exist yet (any first deploy), all three
guards miss simultaneously, all three build the same tag, and the losers of
the race fail at image export with `already exists`, aborting the whole
deploy. Re-running succeeds because the image now exists — but the documented
first-run experience is a hard failure.

Expected: first `devspace deploy` on a clean Docker host succeeds.

## Minimum reproducible example

```bash
docker rmi build-container-localdev 2>/dev/null   # ensure clean state
devspace deploy -n nico-system
```

(Reproduced on Docker 28.4 / buildx v0.24, macOS colima aarch64; the race is
timing-dependent but hit reliably on first deploy here.)

## Relevant log output

```
build:machine-a-tron #18 naming to docker.io/library/build-container-localdev:latest done
build:machine-a-tron #18 ERROR: image "docker.io/library/build-container-localdev:latest": already exists
build:nico-bmc-proxy #18 ERROR: image "docker.io/library/build-container-localdev:latest": already exists
build:machine-a-tron ERROR: failed to build: failed to solve: image "docker.io/library/build-container-localdev:latest": already exists
build_images: build images: error building image machine-a-tron:bXUkIXI: error building image: exit status 1
fatal exit status 1
```

## Suggested fix

Build the shared base image once before the parallel image builds, e.g. a
`before:build` hook:

```yaml
hooks:
  - name: build-base-image
    events: ["before:build"]
    command: |-
      docker image inspect build-container-localdev >/dev/null 2>&1 || \
        docker build --pull=false -t build-container-localdev -f dev/docker/Dockerfile.build-container-x86_64 .
```

and drop the per-image guard from the three build commands. Happy to send a
PR if this is the preferred approach.