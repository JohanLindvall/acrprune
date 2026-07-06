# acrprune

A Go application for cleaning up Azure Container Registries using declarative JSON rules. It supports pruning manifests by tag pattern, age, architecture, and orphan status, as well as deleting entire repositories when they become empty or match bulk-delete criteria.

## Install

```sh
go install github.com/JohanLindvall/acrprune/cmd/acrprune@latest
```

## Authentication

Uses `DefaultAzureCredential` from the Azure SDK (environment variables, managed identity, Azure CLI, etc.).

### ABAC registries and scoped permissions

acrprune works with [ABAC-enabled registries](https://learn.microsoft.com/azure/container-registry/container-registry-rbac-abac-repository-permissions), where permissions are granted per repository rather than registry-wide. The underlying `azcontainerregistry` SDK uses challenge-based authentication, so it automatically requests an ACR access token scoped to exactly the repository each request touches — there is no wildcard-scope requirement and no batch-size tuning to configure.

Two things follow from this:

- **Catalog listing is only used when needed.** When every rule targets a literal repository (`^name$`), acrprune addresses those repositories directly and never lists the catalog, so the `Container Registry Repository Catalog Lister` role is not required. It is only needed when a rule uses a repository regex; if listing is denied, acrprune reports the missing role and suggests switching to literal patterns.
- **Partial access is tolerated.** If a repository-regex rule matches repositories the caller cannot access, acrprune skips each denied repository (logging which were pruned, denied and remaining) instead of aborting the whole run, and exits non-zero at the end with the list of repositories that were denied. To purge only what you own, prefer literal `^repo$` patterns.

## Global Flags

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--registry` | `-r` | *(required)*&nbsp;¹ | Registry name (`myreg`) or full login server (`myreg.azurecr.cn`) |
| `--cache` | `-c` | | Local directory for caching downloaded manifests |
| `--page-size` | `--pagesize` | `250` | Number of items per API page request |
| `--parallelism` | | `16` | Number of concurrent API operations |
| `--verbose` | `-v` | `false` | Enable debug logging |

¹ Required by commands that access the registry; local-only commands such as `top` run without it.

## Commands

### `prune`

Deletes manifests (and empty repositories) according to a JSON rule file. By default runs in dry-run mode. A non-zero exit code is returned on failure.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--input` | `--in`, `--infile` | stdin | Path to a JSON rules file |
| `--dry-run` | `--dryrun` | `true` | When true, only logs what would be deleted |
| `--keep-younger` | `--keepyounger` | `24h` | Grace period — manifests younger than this are never deleted |
| `--include-locked` | `--includelocked` | `false` | Unlock delete/write-disabled manifests and tags before deleting them |

```sh
# Dry run (default)
acrprune -r myregistry -v prune --in rules/delete_untagged_images.json

# Actual deletion
acrprune -r myregistry -v prune --dry-run=false --in rules/cleanup_feature_branches.json

# Read rules from stdin
cat rules/delete_orphaned_manifests.json | acrprune -r myregistry prune --dry-run=false

# Also delete images that have been locked for protection
acrprune -r myregistry -v prune --dry-run=false --include-locked --in rules/cleanup_feature_branches.json
```

#### Locked images

By default a manifest or tag whose `deleteEnabled` or `writeEnabled` attribute has been set to `false` (see [Lock a container image](https://learn.microsoft.com/azure/container-registry/container-registry-image-lock)) cannot be deleted, and the delete will fail. Passing `--include-locked` re-enables delete and write on any locked manifest — and on any locked tag pointing at a manifest being deleted — immediately before deletion. In dry-run mode locked manifests are annotated with `locked=true` in the log but nothing is changed.

**Warning:** `--include-locked` bypasses the image-lock protection mechanism. If unlocking a particular manifest or tag fails, acrprune logs a warning and still attempts the delete.

### `statistics` (alias: `stats`)

Generates per-repository size and manifest statistics as JSON. When writing to a file, the output is rewritten after each repository so partial results survive an interrupted run.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--output` | `-o`, `--out`, `--outfile` | stdout | Output file path |
| `--running` | | | File of running images (output from `get_pod_images.sh`) to annotate stats with usage info |

```sh
acrprune -r myregistry stats --out stats.json
# Sort output by unique bytes:
jq 'sort_by(.unique)' stats.json
# Show repositories with no running pods, sorted by size:
jq '[.[] | select(.running == 0)] | sort_by(.unique)' stats.json
```

The output is a JSON array with one object per repository:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Repository name |
| `unique` | int | Bytes counted once per blob across the whole registry scan (deduplicated contribution) |
| `total` | int | Bytes counting every reference, without cross-repository deduplication |
| `shared` | float | Fraction of `total` bytes shared with other manifests/repos (`1 - unique/total`) |
| `tagged` | int | Number of tagged manifests |
| `untagged` | int | Number of untagged manifests |
| `count` | int | Total number of manifests |
| `newest` | timestamp | Last-updated time of the newest manifest |
| `oldest` | timestamp | Last-updated time of the oldest manifest |
| `running` | int | Number of tagged manifests matching a keep rule from `--running` (always `0` without it) |

### `generate`

Reads a list of `registry.azurecr.io/repo:tag` image references from stdin and produces a JSON rule file that keeps only those images (deleting everything else in matching repos).

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--output` | `--out`, `--outfile` | stdout | Output file path |

```sh
scripts/get_pod_images.sh | acrprune -r myregistry generate | acrprune -r myregistry -v prune --dry-run=false
```

### `top`

Reads a statistics JSON file (as produced by `statistics`) and prints the top repositories as an aligned table, with sizes in human-readable form and `shared` as a percentage. Runs entirely locally — no registry access or `--registry` flag needed. The input file may also be given as a positional argument.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--input` | `--in`, `--infile` | stdin | Statistics JSON file |
| `--sort` | `-s` | `unique` | Sort key: `count`, `name`, `newest`, `oldest`, `running`, `shared`, `tagged`, `total`, `unique`, `untagged` |
| `--top` | `-k`, `-n` | `20` | Number of rows to print (`0` for all) |

Sizes and counts sort descending (largest first), `newest` most-recent-first, `oldest` oldest-first, and `name` alphabetically.

```sh
# Top 20 repositories by unique (deduplicated) size
acrprune top stats.json

# Top 10 by total size
acrprune top stats.json -s total -k 10

# Straight from a fresh scan
acrprune -r myregistry stats | acrprune top -s shared
```

```text
NAME        UNIQUE  TOTAL   SHARED  TAGGED  UNTAGGED  COUNT  RUNNING  NEWEST      OLDEST
runner      25 GB   66 GB   62.6%   53      106       159    0        2026-06-15  2025-06-01
gp-profile  11 GB   29 GB   63.5%   75      150       225    0        2026-06-23  2025-11-27
```

## Rule File Format

Rules are a JSON array of repository rules. Each rule matches repositories by regex and defines how to handle tagged and untagged manifests. Regexes are validated when the rule file is loaded.

```json
[
  {
    "description": "Human-readable description (optional)",
    "repo": "<regex matching repository names>",
    "ignore_missing_manifests": true,
    "delete_orphaned_manifests": false,
    "must_delete_everything": false,
    "untagged": [ /* untagged rules */ ],
    "tagged": [ /* tagged rules */ ]
  }
]
```

### Repository Rule Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `description` | string | | Optional description |
| `repo` | string | | Regex to match repository names |
| `ignore_missing_manifests` | bool | `true` | Ignore (rather than fail on) manifests that can't be downloaded |
| `delete_orphaned_manifests` | bool | `false` | Delete manifests whose dependencies are missing |
| `must_delete_everything` | bool | `false` | If any manifest must be kept, keep all (used for "delete entire repo" rules) |

### Tagged Rule Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `tag` | string | *(match all)* | Regex to match tag names |
| `arch` | string | *(match all)* | Regex to match architecture (e.g. `amd64`, `arm64`) |
| `newest` | int | | Keep/match only the N newest manifests. Negative value excludes the N newest |
| `match_newer` | string | | Match manifests newer than this duration (e.g. `24h`, `30d`) |
| `match_older` | string | | Match manifests older than this duration |
| `keep` | bool | `true` | Whether matching manifests are kept or deleted |

### Untagged Rule Fields

Same as tagged rules but without the `tag` field.

### Rule Evaluation

Rules are evaluated in order. The first matching rule determines whether a manifest is kept or deleted. If no rule matches, the manifest is kept. Duration values support Go duration syntax (`24h`, `168h`) and extended syntax with days and weeks (`14d`, `2w`).

## Included Rule Examples

| File | Description |
|------|-------------|
| `rules/cleanup_feature_branches.json` | Deletes feature branch/PR images older than 14 days |
| `rules/delete_untagged_images.json` | Deletes untagged manifests older than 24 hours |
| `rules/delete_orphaned_manifests.json` | Deletes manifests with missing dependencies |
| `rules/delete_old_repos.json` | Deletes all content from repos where everything is older than 730 days |
| `rules/delete_amd64_only_images.json` | Deletes repos that contain only amd64 images (no arm64) |

## Behaviour Notes

- Repositories that have no manifests after download are deleted entirely (respecting `--dry-run`).
- Manifests with a `subject` field (e.g. signatures, attestations) are always kept.
- The `--keep-younger` grace period overrides rule decisions — recently updated manifests are never deleted.
- When every rule targets a literal repository name (`^name$`), only those repositories are fetched instead of listing the whole registry.
- Cached manifests are stored under `<cache>/<registry>/` and are removed from cache when deleted from the registry.

## Package Layout

| Package | Responsibility |
|---------|----------------|
| `cmd/acrprune` | CLI wiring (flags, commands, I/O) |
| `internal/rules` | JSON rule format, validation/compilation, rule generation from image lists |
| `internal/registry` | ACR data-plane access: paging, parallel manifest download, on-disk cache |
| `internal/pruner` | Rule evaluation, orphan detection, keep/delete decisions, statistics |

## Similar Work

- https://github.com/Azure/acr-cli

This however lacks the rules and robustness, making it unusable for larger registries.
