Title: bug(devspace): image-load hook fails for kind clusters not named "kind"

## Version

main (reproduced at a171cb1, still present at 7a199a7)

_Context: hit while standing up the DevSpace local-dev environment to evaluate
NICo — I work on DPU/bare-metal provisioning at Crusoe (an NVIDIA Cloud
Partner)._

## Describe the bug

The `load-images-into-local-cluster` hook in `devspace.yaml` (added in #960)
detects any `kind-*` kube context, but then calls `kind load docker-image`
without `--name`. `kind` defaults to the cluster named `kind`, so
`devspace deploy` fails for a kind cluster with any other name — even though
the context match (`kind-*`) shows non-default names are intended to be
supported.

Expected: images are loaded into the cluster of the current kube context.

## Minimum reproducible example

```bash
kind create cluster --name nico          # context becomes kind-nico
dev/deployment/devspace/bootstrap-prereqs.sh
devspace deploy -n nico-system
```

## Relevant log output

```
Execute hook 'load-images-into-local-cluster' at before:deploy
Loading images into kind cluster...
ERROR: no nodes found for cluster "kind"
fatal exit status 1
```

## Suggested fix

Derive the cluster name from the context (verified working locally):

```diff
         kind-*)
           echo "Loading images into kind cluster..."
-          kind load docker-image \
+          kind load docker-image --name "${CONTEXT#kind-}" \
```

Happy to send a PR if this is the preferred approach.