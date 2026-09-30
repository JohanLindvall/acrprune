# acrprune

A Go application for cleaning up Azure Container Registry (ACR) and GitHub Container Registry (GHCR, `ghcr.io`) using declarative JSON rules. It supports pruning manifests by tag pattern, age, architecture, and orphan status, as well as deleting entire repositories when they become empty or match bulk-delete criteria.

The registry is chosen with `--registry`: an ACR registry name (`myreg`) or login server (`myreg.azurecr.cn`), or `ghcr.io/<owner>` for the container packages of a GitHub user or organization.

## Install

```sh
go install github.com/JohanLindvall/acrprune/cmd/acrprune@latest
```

## Authentication

### Azure Container Registry

Uses `DefaultAzureCredential` from the Azure SDK (environment variables, managed identity, Azure CLI, etc.).

#### ABAC registries and scoped permissions

acrprune works with [ABAC-enabled registries](https://learn.microsoft.com/azure/container-registry/container-registry-rbac-abac-repository-permissions), where permissions are granted per repository rather than registry-wide. The underlying `azcontainerregistry` SDK uses challenge-based authentication, so it automatically requests an ACR access token scoped to exactly the repository each request touches — there is no wildcard-scope requirement and no batch-size tuning to configure.

Two things follow from this:

- **Catalog listing is only used when needed.** When every rule targets a literal repository (`^name$`), acrprune addresses those repositories directly and never lists the catalog, so the `Container Registry Repository Catalog Lister` role is not required. It is only needed when a rule uses a repository regex; if listing is denied, acrprune reports the missing role and suggests switching to literal patterns.
- **Partial access is tolerated.** If a repository-regex rule matches repositories the caller cannot access, acrprune skips each denied repository (logging which were pruned, denied and remaining) instead of aborting the whole run, and exits non-zero at the end with the list of repositories that were denied. When *every* repository is denied, the error suggests checking the credential itself. The `statistics` command behaves the same way: denied repositories are skipped, the statistics for the accessible ones are still written, and the exit code is non-zero. To purge only what you own, prefer literal `^repo$` patterns.

### GitHub Container Registry

Uses a GitHub token, taken from the first of:

1. `GH_TOKEN`
2. `GITHUB_TOKEN`
3. the [GitHub CLI](https://cli.github.com/)'s login (`gh auth token`)

The token needs the `read:packages` scope, and `delete:packages` to prune. GitHub's packages API accepts [personal access tokens (classic)](https://docs.github.com/en/packages/learn-github-packages/about-permissions-for-github-packages#about-scopes-and-permissions-for-package-registries), not fine-grained ones; a GitHub CLI login gets the scopes with `gh auth refresh --scopes read:packages,delete:packages`. Deleting requires admin access to the package; a package the token may read but not delete is skipped and reported like a denied ACR repository.

```sh
export GH_TOKEN=ghp_...
acrprune -r ghcr.io/myorg -v prune --in rules/delete_untagged_images.json
```

## Global Flags

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--registry` | `-r` | *(required)*&nbsp;¹ | ACR registry name (`myreg`) or login server (`myreg.azurecr.cn`), or `ghcr.io/<owner>` |
| `--cache` | `-c` | | Local directory for caching downloaded manifests |
| `--page-size` | `--pagesize` | `250` | Number of items per API page request (at least 1; GHCR caps it at 100) |
| `--parallelism` | | `16` | Number of concurrent API operations (at least 1) |
| `--verbose` | `-v` | `false` | Enable debug logging |

¹ Required by every command except `top`, which runs locally.

## Commands

### `prune`

Deletes manifests (and empty repositories) according to a JSON rule file. By default runs in dry-run mode. A non-zero exit code is returned on failure.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--input` | `--in`, `--infile` | stdin | Path to a JSON rules file |
| `--dry-run` | `--dryrun` | `true` | When true, only logs what would be deleted |
| `--keep-younger` | `--keepyounger` | `24h` | Grace period — manifests younger than this are never deleted |
| `--include-locked` | `--includelocked` | `false` | Unlock delete/write-disabled manifests and tags before deleting them (ACR only) |

```sh
# Dry run (default)
acrprune -r myregistry -v prune --in rules/delete_untagged_images.json

# Actual deletion
acrprune -r myregistry -v prune --dry-run=false --in rules/cleanup_feature_branches.json

# Read rules from stdin
cat rules/delete_orphaned_manifests.json | acrprune -r myregistry prune --dry-run=false

# Also delete images that have been locked for protection
acrprune -r myregistry -v prune --dry-run=false --include-locked --in rules/cleanup_feature_branches.json

# Prune the container packages of a GitHub organization
acrprune -r ghcr.io/myorg -v prune --dry-run=false --in rules/cleanup_feature_branches.json
```

#### Locked images

By default a manifest or tag whose `deleteEnabled` or `writeEnabled` attribute has been set to `false` (see [Lock a container image](https://learn.microsoft.com/azure/container-registry/container-registry-image-lock)) cannot be deleted, and the delete will fail. Passing `--include-locked` re-enables delete and write on any locked manifest — and on any locked tag pointing at a manifest being deleted — immediately before deletion. In dry-run mode locked manifests are annotated with `locked=true` in the log but nothing is changed.

**Warning:** `--include-locked` bypasses the image-lock protection mechanism. If unlocking a particular manifest or tag fails, acrprune logs a warning and still attempts the delete.

GHCR has no image locks; there the flag has no effect.

### `statistics` (alias: `stats`)

Generates per-repository size and manifest statistics as JSON. When writing to a file, the output is rewritten after each repository so partial results survive an interrupted run.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--output` | `-o`, `--out`, `--outfile` | stdout | Output file path |
| `--running` | | | File of running images (output from `get_pod_images.sh`) to annotate stats with usage info |

```sh
acrprune -r myregistry stats --out stats.json
acrprune -r ghcr.io/myorg stats --out stats.json
# Sort output by unique bytes:
jq 'sort_by(.unique)' stats.json
# Show repositories with no running pods, sorted by size:
jq '[.[] | select(.running == 0)] | sort_by(.unique)' stats.json
```

The output is a JSON array with one object per repository:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Repository name |
| `unique` | int | Bytes counted once per blob (manifest document, config and layers) across the whole registry scan (deduplicated contribution) |
| `total` | int | Bytes counting every reference, without cross-repository deduplication |
| `shared` | float | Fraction of `total` bytes shared with other manifests/repos (`1 - unique/total`) |
| `tagged` | int | Number of tagged manifests |
| `untagged` | int | Number of untagged manifests |
| `count` | int | Total number of manifests |
| `newest` | timestamp | Last-updated time of the newest manifest |
| `oldest` | timestamp | Last-updated time of the oldest manifest |
| `running` | int | Number of manifests (tagged or digest-pinned) matching a keep rule from `--running` (always `0` without it) |

### `generate`

Reads a list of image references from stdin and produces a JSON rule file that keeps only those images (deleting everything else in matching repos). Tag references (`myreg.azurecr.io/repo:tag`, `ghcr.io/myorg/repo:tag`) are kept by tag; digest-pinned references (`myreg.azurecr.io/repo@sha256:…`) are kept by digest, whether or not the manifest is tagged in the registry; a reference carrying both keeps both, since the digest pins what is actually running even if the tag has been moved. References for other registries — and, on GHCR, other owners — are ignored. The input need not be sorted or deduplicated: each repository yields exactly one rule. `generate` works offline: it needs `--registry` to know which references are its images, but no credentials.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--output` | `--out`, `--outfile` | stdout | Output file path |

```sh
scripts/get_pod_images.sh | acrprune -r myregistry generate | acrprune -r myregistry -v prune --dry-run=false
scripts/get_pod_images.sh | acrprune -r ghcr.io/myorg generate | acrprune -r ghcr.io/myorg -v prune --dry-run=false
```

### `top`

Reads a statistics JSON file (as produced by `statistics`) and prints the top repositories as an aligned table, with sizes in human-readable form and `shared` as a percentage. Runs entirely locally — no registry access or `--registry` flag needed. The input file may also be given as a positional argument (but not both, and at most one).

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
| `repo` | string | | Regex to match repository names (on GHCR, package names: `team/app` for `ghcr.io/myorg/team/app`) |
| `ignore_missing_manifests` | bool | `true` | Ignore (rather than fail on) manifests that are listed but can't be downloaded; ignored manifests are left in place |
| `delete_orphaned_manifests` | bool | `false` | Delete manifests whose dependencies are missing |
| `must_delete_everything` | bool | `false` | If any manifest must be kept, keep all (used for "delete entire repo" rules) |

### Tagged Rule Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `tag` | string | *(match all)* | Regex to match tag names |
| `arch` | string | *(match all)* | Regex to match architecture (e.g. `amd64`, `arm64`) |
| `os` | string | *(match all)* | Regex to match operating system (e.g. `linux`, `windows`) |
| `digest` | string | *(match all)* | Regex to match the manifest digest (e.g. `^sha256:3b1a…$`), for keeping digest-pinned images |
| `newest` | int | | Match only the N newest of the manifests this rule matches. Negative value excludes the N newest |
| `match_newer` | string | | Match manifests newer than this duration (e.g. `24h`, `30d`) |
| `match_older` | string | | Match manifests older than this duration |
| `keep` | bool | `true` | Whether matching manifests are kept or deleted |

### Untagged Rule Fields

Same as tagged rules but without the `tag` field.

### Rule Evaluation

Rules are evaluated in order. The first matching rule determines whether a manifest is kept or deleted. If no rule matches, the manifest is kept. Duration values support Go duration syntax (`24h`, `168h`) and extended syntax with days and weeks (`14d`, `2w`).

`newest` ranks a manifest against the other manifests **the same rule matches**, not against the whole repository. So the pair of rules below keeps the three most recent release images however many newer feature-branch images sit alongside them:

```json
"tagged": [
  { "tag": "^release-", "newest": 3, "keep": true },
  { "tag": ".+", "keep": false }
]
```

Manifests of equal age are ranked by digest, so repeated runs over an unchanged repository always decide the same way.

## Included Rule Examples

| File | Description |
|------|-------------|
| `rules/cleanup_feature_branches.json` | Deletes feature branch/PR images older than 14 days |
| `rules/delete_untagged_images.json` | Deletes untagged manifests older than 24 hours |
| `rules/delete_orphaned_manifests.json` | Deletes manifests with missing dependencies |
| `rules/delete_old_repos.json` | Deletes all content from repos where everything is older than 730 days |
| `rules/delete_amd64_only_images.json` | Deletes repos that contain only amd64 images (no arm64) |

## Behaviour Notes

- A repository the rules leave no manifest in is deleted entirely (respecting `--dry-run`) — unless it lists a manifest that could not be downloaded. The rules never judged that manifest, so it is left in place, and the rest of the repository is deleted manifest by manifest.
- Manifests with a `subject` field (signatures, attestations, SBOMs) follow their subject instead of matching rules: they are kept exactly as long as the subject is kept, deleted along with it, and deleted when the subject is already gone. They do not occupy `newest` ranking slots, and they do not count as "kept" for `must_delete_everything`, so signed repositories can still be bulk-deleted.
- The `--keep-younger` grace period overrides rule decisions — recently updated manifests are never deleted. This holds for subject-bearing manifests too, so a dangling signature outlives its subject by at most the grace period.
- Listings are a point-in-time snapshot: tags moved or manifests pushed while a prune is running are not observed. New pushes are protected by `--keep-younger`, except in a repository the run deletes entirely; avoid retagging old manifests during a run.
- The per-repository byte counts logged by `prune` deduplicate blobs within the repository only; layers shared with other repositories may not actually be freed by the registry's garbage collector.
- A manifest the registry reports no last-updated time for is never deleted, since every age-based rule would otherwise read it as infinitely old.
- A manifest is orphaned when something it references is missing — an index child or a `subject` alike — and the flag propagates both up to the indexes referencing it and down to its children. A child still reachable through a healthy index is not orphaned by a broken sibling.
- When every rule targets a literal repository name (`^name$`), only those repositories are fetched instead of listing the whole registry. A pattern containing an active metacharacter is not a literal name: `^my.repo$` matches `myXrepo` too, so it is resolved by listing the catalog. Write `^my\.repo$` (what `generate` emits) to address a repository with a dot in its name directly.
- Cached manifests are stored under `<cache>/<registry>/` and are removed from cache when deleted from the registry. Image configs read for `arch`/`os` rules on GHCR are cached alongside them. Cached content is verified against its digest on every read.

### GitHub Container Registry

- Repositories are the owner's container packages and manifests are their package versions. ghcr.io's registry API can neither list a repository's untagged manifests nor delete anything, so acrprune lists and deletes through the [GitHub REST API](https://docs.github.com/en/rest/packages/packages) and downloads manifest content from ghcr.io itself. Deleting a manifest deletes its package version, with every tag pointing at it.
- A multi-platform push stores its platform images and attestations as separate, *untagged* package versions. They are kept as dependencies of the index that references them, so cleaning up untagged versions does not break multi-platform images.
- GHCR refuses to delete a package's last tagged version ("You must delete the package instead"). When the rules would keep only untagged manifests of a package — a digest-pinned running image, say — acrprune keeps the newest tagged image as well, with its dependencies and signatures, and logs a warning. When the rules keep nothing, the whole package is deleted. Deleted packages and versions can be [restored](https://docs.github.com/en/packages/learn-github-packages/deleting-and-restoring-a-package) for 30 days.
- GHCR reports no image platforms. Images referenced by an index take theirs from the index; for single-platform images, rules using `arch` or `os` download the image config (once per distinct config, cached). Rules not matching on platform download nothing extra.
- The last-updated time is the package version's `updated_at`.
- GitHub rate limits are honoured: throttled requests wait as long as GitHub asks (`Retry-After`, or until the limit resets) and are retried, with a warning logged, for up to two hours per request — long enough to ride out GitHub's hourly limits during a large cleanup. Server and network errors are retried a few times with exponential backoff.
- GitHub does not let a public package be deleted, in whole or in part, once one of its versions has been downloaded more than 5,000 times; acrprune reports GitHub's error for such a package.

## Package Layout

| Package | Responsibility |
|---------|----------------|
| `cmd/acrprune` | CLI wiring (flags, commands, I/O, credentials) |
| `internal/rules` | JSON rule format, validation/compilation, rule generation from image lists |
| `internal/registry` | Registry-neutral access: `--registry` parsing, manifest model, parallel manifest download with digest verification, on-disk cache, platform resolution, deletion; the `Backend` interface a registry API implements |
| `internal/registry/acr` | `Backend` for Azure Container Registry, on the `azcontainerregistry` data-plane client |
| `internal/registry/ghcr` | `Backend` for GitHub Container Registry: GitHub REST packages API, ghcr.io registry token exchange, rate-limit-aware retries |
| `internal/registry/registrytest` | In-memory `Backend` for tests |
| `internal/pruner` | Rule evaluation, orphan detection, keep/delete decisions, statistics |

## Similar Work

- https://github.com/Azure/acr-cli

This however lacks the rules and robustness, making it unusable for larger registries.

## License

[MIT](LICENSE)
