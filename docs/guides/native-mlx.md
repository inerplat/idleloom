# MLX Workloads

Use the Native backend for direct Apple Metal execution through MLX. Complete
the [Native Metal quick start](../getting-started/native-metal.md) first.

## Training

```sh
idlectl recipe render train/mlx-linear-regression@v1 \
  --name native-train \
  -o yaml > native-train.yaml

kubectl -n "${IDLELOOM_NAMESPACE}" apply -f native-train.yaml
kubectl -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-train \
  --timeout=15m

idlectl logs workload/native-train \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

The result includes device information, bounded metrics, converged parameters,
and a checkpoint artifact. See the [Native training contract](../native-training.md)
for custom source, metrics, artifacts, retry, and resume behavior.

## Batch inference

```sh
idlectl recipe render infer/mlx-batch@v1 \
  --name native-mlx-infer \
  -o yaml > native-mlx-infer.yaml

kubectl -n "${IDLELOOM_NAMESPACE}" apply -f native-mlx-infer.yaml
kubectl -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-mlx-infer \
  --timeout=20m

idlectl logs workload/native-mlx-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

The first execution may download and verify the locked runtime and model. A
successful log ends with the structured result envelope.

## Serving

Serving requires a WireKube-connected host and a real Linux kubelet node on the
same mesh for the client Pod:

```sh
idlectl recipe render serve/mlx-qwen@v1 \
  --name native-mlx-serve \
  -o yaml > native-mlx-serve.yaml

kubectl -n "${IDLELOOM_NAMESPACE}" apply -f native-mlx-serve.yaml
kubectl -n "${IDLELOOM_NAMESPACE}" wait --for=condition=Ready \
  idleloomworkload/native-mlx-serve --timeout=20m

kubectl apply -f "${IDLELOOM_REPO}/examples/native/serve-mlx-client.yaml"
kubectl -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/native-mlx-serve-client --timeout=10m
kubectl -n default logs pod/native-mlx-serve-client
```

Read [Native Serving](native-serving.md) for the Service, EndpointSlice, and
connectivity contract.

## Bring your own MLX model

The MLX runtime can serve and batch-run any MLX safetensors repository on
Hugging Face, not only the built-in Qwen model. The catalog entry pins one
commit and the digest of its files, so the bytes a host runs never change after
the entry is applied.

The model must load with the locked runtime (mlx 0.32.0 and mlx-lm 0.31.3), so
its architecture must be one that mlx-lm release supports. The repository must
be public; the agent sends no access token. It must contain a `config.json`,
and a `config.json` that names a `model_file` is refused, because mlx-lm would
execute that Python module while loading the model.

### Pin a commit

Branches and tags move, so the catalog accepts only a full 40-character commit
SHA. Resolve the branch you reviewed to its commit:

```sh
export HF_REPO=OWNER/NAME
export HF_REVISION="$(git ls-remote "https://huggingface.co/${HF_REPO}" refs/heads/main | cut -f1)"
echo "${HF_REVISION}"
```

### Generate the catalog entry

`idlectl model inspect hf` reads the commit's file list and prints a
ready-to-apply `IdleloomModel` with the computed `manifestDigest` and
`sizeBytes`:

```sh
idlectl model inspect hf "${HF_REPO}@${HF_REVISION}" \
  --name my-mlx-model \
  --max-context 8192 > my-mlx-model.yaml

kubectl apply --dry-run=server -f my-mlx-model.yaml
kubectl apply -f my-mlx-model.yaml
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--name` | required | `IdleloomModel` name, a DNS subdomain. Recipes refer to it through `model`. |
| `--max-context` | `4096` | Largest context the model is admitted with, up to 131072. |
| `--min-memory` | conservative estimate | `minimumUnifiedMemory`. It must be at least the artifact size plus 512 MiB. |
| `--endpoint` | `https://huggingface.co` | Origin to list and hash from. Agents always download from `https://huggingface.co`, so a mirror must serve identical files. |

The command downloads no weights. For files stored with Git LFS it uses the
SHA-256 the tree API reports. It downloads the small files stored without LFS,
checks them against their git blob ID, and hashes them. A file stored without
LFS that is larger than 16 MiB has no SHA-256 the command can learn without
downloading it, so the command stops and names the file.

The printed entry uses family `mlx`, runtime profile `mlx-lm-v1`, format
`mlx-safetensors-v1`, and an unsigned `huggingFace` source:

```yaml
apiVersion: ai.idleloom.io/v1alpha1
kind: IdleloomModel
metadata:
  name: my-mlx-model
spec:
  family: mlx
  runtimeProfile: mlx-lm-v1
  artifact:
    huggingFace:
      repository: OWNER/NAME
      revision: 40-character commit SHA
    manifestDigest: sha256:...
    format: mlx-safetensors-v1
    sizeBytes: 4500000000
  minimumUnifiedMemory: "..."
  maxContextLength: 8192
  maxConcurrentRequests: 1
```

### Manifest digest

The manifest covers every regular file at the commit whose base name does not
start with `.`, so `.gitattributes` and similar files are left out. Files are
sorted by path in byte order, and each contributes one line with no header:

```text
path<TAB>size<TAB>sha256hex<LF>
```

`manifestDigest` is `sha256:` followed by the SHA-256 of that text. Paths must
be clean relative paths of printable ASCII, and paths that differ only in case
are refused because they collide on the default macOS filesystem. You can
recompute the digest from a git checkout of the commit with its LFS files
fetched. A download tool's local cache directory adds metadata files of its own
and gives a different digest.

```sh
find . -type f ! -name '.*' ! -path './.git/*' | sed 's|^\./||' | LC_ALL=C sort |
  while IFS= read -r file; do
    printf '%s\t%s\t%s\n' "${file}" "$(stat -f %z "${file}")" "$(shasum -a 256 "${file}" | cut -d' ' -f1)"
  done | shasum -a 256 | sed 's/^/sha256:/; s/ .*//'
```

### First use and disk space

Any host that has the MLX runtime ready, or can prepare it, accepts the model.
The host does not need the model in advance. On the first assignment the agent
prepares the locked runtime, lists the pinned commit, and refuses the model
before downloading any weights if the files do not hash to `manifestDigest` or
do not add up to `sizeBytes`. It then downloads each file and checks its size
and SHA-256, renewing the assignment lease while it does so. A large model can
take a long time to download, so give the first `kubectl wait` a generous
timeout.

Each model is stored once per manifest digest under the agent's `--root`, in
`models/hf/<digest>` with its receipt next to it in `models/hf/<digest>.json`.
The receipt lists every commit verified to produce that manifest, so a fork or
a commit that changed only dotfiles reuses the same files after a fresh
listing and rehash.
Plan for `sizeBytes` of free space per model in addition to the runtime.
Prepared models are not removed automatically; delete both paths while no
workload uses the model to reclaim the space.

Before every start the agent rehashes the whole model directory and refuses to
run if any file differs or any file the manifest does not name has appeared.
The runtime's sandbox can read that one model directory and none of the other
prepared models.
Heartbeats check only file shapes against the receipt, and prepared models are
listed in the host's `status.availableModels`.

Generic MLX models have no measured memory profile. Admission charges the
larger of the workload request and the model's `minimumUnifiedMemory`, which
defaults to the conservative estimate `artifact bytes + 4 GiB + maxContextLength
* 1 MiB`.

### Run it with the MLX recipes

The `serve/mlx-qwen@v1` and `infer/mlx-batch@v1` recipes take the catalog entry
through their `model` parameter:

```sh
cat > my-mlx-values.yaml <<EOF
model: my-mlx-model
prompt: Explain why idle compute is useful in one sentence.
maxTokens: 256
EOF

idlectl recipe render infer/mlx-batch@v1 \
  --name my-mlx-infer \
  --values my-mlx-values.yaml \
  -o yaml > my-mlx-infer.yaml
kubectl -n "${IDLELOOM_NAMESPACE}" apply -f my-mlx-infer.yaml

cat > my-mlx-serve-values.yaml <<EOF
model: my-mlx-model
modelAlias: my-mlx-model
EOF

idlectl recipe render serve/mlx-qwen@v1 \
  --name my-mlx-serve \
  --values my-mlx-serve-values.yaml \
  -o yaml > my-mlx-serve.yaml
kubectl -n "${IDLELOOM_NAMESPACE}" apply -f my-mlx-serve.yaml
```

`maxTokens` must stay below the model's `maxContextLength`. The projected Pod
records the model as `hf://OWNER/NAME@REVISION#sha256:...`.

## Cleanup

```sh
kubectl -n default delete pod/native-mlx-serve-client --ignore-not-found
kubectl -n "${IDLELOOM_NAMESPACE}" delete -f native-mlx-serve.yaml --ignore-not-found
kubectl -n "${IDLELOOM_NAMESPACE}" delete -f native-mlx-infer.yaml --ignore-not-found
kubectl -n "${IDLELOOM_NAMESPACE}" delete -f native-train.yaml --ignore-not-found
kubectl -n "${IDLELOOM_NAMESPACE}" delete -f my-mlx-serve.yaml -f my-mlx-infer.yaml --ignore-not-found
kubectl delete -f my-mlx-model.yaml --ignore-not-found
rm -f native-mlx-serve.yaml native-mlx-infer.yaml native-train.yaml \
  my-mlx-model.yaml my-mlx-values.yaml my-mlx-serve-values.yaml my-mlx-serve.yaml my-mlx-infer.yaml
```
