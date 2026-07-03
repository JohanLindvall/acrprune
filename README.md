# acrprune

A Go application for cleaning up Azure Container Registries using declarative JSON rules. It supports pruning manifests by tag pattern, age, architecture, and orphan status, as well as deleting entire repositories when they become empty or match bulk-delete criteria.

## Install

```sh
go install github.com/JohanLindvall/acrprune/cmd/acrprune@latest
```

## Authentication

Uses `DefaultAzureCredential` from the Azure SDK (environment variables, managed identity, Azure CLI, etc.).

## Global Flags

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--registry` | `-r` | *(required)* | Registry name (`myreg`) or full login server (`myreg.azurecr.cn`) |
| `--cache` | `-c` | | Local directory for caching downloaded manifests |
| `--page-size` | `--pagesize` | `250` | Number of items per API page request |
| `--parallelism` | | `16` | Number of concurrent API operations |
| `--verbose` | `-v` | `false` | Enable debug logging |

## Commands

### `prune`

Deletes manifests (and empty repositories) according to a JSON rule file. By default runs in dry-run mode. A non-zero exit code is returned on failure.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--input` | `--in`, `--infile` | stdin | Path to a JSON rules file |
| `--dry-run` | `--dryrun` | `true` | When true, only logs what would be deleted |
| `--keep-younger` | `--keepyounger` | `24h` | Grace period — manifests younger than this are never deleted |

```sh
# Dry run (default)
acrprune -r myregistry -v prune --in rules/delete_untagged_images.json

# Actual deletion
acrprune -r myregistry -v prune --dry-run=false --in rules/cleanup_feature_branches.json

# Read rules from stdin
cat rules/delete_orphaned_manifests.json | acrprune -r myregistry prune --dry-run=false
```

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
