# crprune

A Go tool that cleans up Azure Container Registry (ACR) and GitHub Container Registry (GHCR, `ghcr.io`) with declarative JSON rules. It prunes manifests by tag pattern, age, architecture and orphan status, and deletes entire repositories that become empty or match bulk-delete criteria.

`--registry` chooses the registry: an ACR registry name (`myreg`) or login server (`myreg.azurecr.io`, or `myreg.azurecr.cn` / `myreg.azurecr.us` in Azure's sovereign clouds, optionally prefixed with `https://`), or `ghcr.io/<owner>` for the container packages of a GitHub user or organization. Other hosts are refused (see [Azure Container Registry](#azure-container-registry)).

## Install

Install the latest release binary into `~/.local/bin`:

```sh
curl -fsSL https://github.com/JohanLindvall/crprune/releases/latest/download/download.sh | sh -s -- ~/.local/bin
```

The script, [`scripts/download.sh`](scripts/download.sh), downloads the release archive for this machine (Linux amd64 or arm64), verifies it against the release's SHA-256 checksums, and replaces `crprune` in the given directory (default: the current one) atomically. It needs curl or GNU Wget, `tar`, and `sha256sum` or `shasum`. `-v VERSION` installs a specific release, e.g. `sh -s -- -v v0.1.13 ~/.local/bin`; releases from before the rename (v0.1.12 and earlier) install as `acrprune`. Set `GH_TOKEN` or `GITHUB_TOKEN` when GitHub's API rate limit for anonymous requests gets in the way, as on shared CI runners. For a system-wide install, run `sudo sh -s -- /usr/local/bin` instead.

To build from source instead (requires Go 1.26 or later):

```sh
go install github.com/JohanLindvall/crprune/cmd/crprune@latest
```

`crprune --version` prints the version `make build` and the release archives set, or else the module version Go recorded, such as `v0.2.0` for `go install …@v0.2.0`.

## Authentication

### Azure Container Registry

Uses the Azure SDK's `DefaultAzureCredential` (environment variables, managed identity, Azure CLI, etc.).

The registry token a credential obtains is valid for every registry the identity can reach, so crprune sends Azure credentials only to ACR login servers: a registry name, or a host ending in `.azurecr.io`, `.azurecr.cn` or `.azurecr.us`, dedicated data endpoints such as `myreg-abc123.azurecr.io` included. Any other host is refused, look-alikes and typos like `myreg.azurecr.co` included. List another cloud's or a private suffix explicitly, comma-separated: `CRPRUNE_ACR_SUFFIXES=.azurecr.de`.

For Azure China (`.azurecr.cn`) and Azure Government (`.azurecr.us`) login servers, environment, workload identity and other Entra ID credentials authenticate against that cloud's authority. `AZURE_AUTHORITY_HOST` chooses the authority explicitly and takes precedence. The Azure CLI credential follows `az cloud set` either way.

#### ABAC registries and scoped permissions

crprune works with [ABAC-enabled registries](https://learn.microsoft.com/azure/container-registry/container-registry-rbac-abac-repository-permissions), which grant permissions per repository. The `azcontainerregistry` SDK authenticates by challenge, requesting an access token scoped to exactly the repository each request touches: no wildcard scope is required, and there is no batch size to tune. So:

- **The catalog is listed only when needed.** When every rule targets a literal repository (`^name$`), crprune addresses those repositories directly, and the `Container Registry Repository Catalog Lister` role is not required. Only a repository regex needs the listing; if it is denied, crprune reports the missing role and suggests literal patterns.
- **Partial access is tolerated.** A repository the caller cannot access is skipped, logging how many were pruned, denied and remaining, instead of aborting the run, which exits non-zero at the end listing the denied repositories. When *every* repository is denied, the error suggests checking the credential itself. `statistics` likewise skips denied repositories, still writes the others' statistics, and exits non-zero. To purge only what you own, prefer literal `^repo$` patterns.

### GitHub Container Registry

Uses a GitHub token: `GH_TOKEN`, else `GITHUB_TOKEN`, else the [GitHub CLI](https://cli.github.com/)'s login (`gh auth token`).

The token needs the `read:packages` scope, and `delete:packages` to prune. The packages API accepts [personal access tokens (classic)](https://docs.github.com/en/packages/learn-github-packages/about-permissions-for-github-packages#about-scopes-and-permissions-for-package-registries), not fine-grained ones. Classic tokens and GitHub CLI logins report their scopes, which crprune checks before scanning: it refuses a token without `read:packages` (or `write:packages`, which includes it), and, for `prune --dry-run=false`, one without `delete:packages`. `gh auth refresh --scopes read:packages,delete:packages` gives a GitHub CLI login both. GitHub App tokens, such as a workflow's `GITHUB_TOKEN`, report no scopes; their permissions show only in use.

Deleting requires admin access to the package. GitHub answers a deletion without it with 404, as if the version were already gone, so crprune looks the version or package up again and counts a 404 as deleted only when it really is gone; the package is skipped and reported like a denied ACR repository. In GitHub Actions, grant the job `permissions: packages: write`, and give the workflow's repository the Admin role in each package's *Manage Actions access* settings, so that its `GITHUB_TOKEN` may delete.

```sh
export GH_TOKEN=ghp_...
crprune -r ghcr.io/myorg -v prune --in rules/delete_untagged_images.json
```

## Global Flags

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--registry` | `-r` | *(required)*&nbsp;¹ | ACR registry name or login server, or `ghcr.io/<owner>` |
| `--cache` | `-c` | | Directory caching downloaded manifests (see [Behaviour Notes](#behaviour-notes)) |
| `--page-size` | `--pagesize` | `250` | Items per API page request (at least 1; GHCR caps it at 100) |
| `--parallelism` | | `16` | Concurrent API operations (at least 1) |
| `--progress` | | `auto` | Batch display: `auto` (TUI on a terminal), `plain` (logs), or `tui` (require a terminal) |
| `--verbose` | `-v` | `false` | Enable debug logging |
| `--version` | | | Print the version |

¹ Required by every command except `top`, which runs locally.

### Interactive batch progress

`prune` and `statistics` show a live dashboard when stderr is a terminal, drawn with [tcell](https://github.com/gdamore/tcell) directly, without a widget framework. It shows the registry, a prominent **DRY RUN / LIVE DELETE** indicator, repository progress, the current inspection phase, manifest and cache counts, keep/delete decisions (for `statistics`, manifest counts and deduplicated bytes), estimated bytes, warnings, and API retry countdowns.

```sh
crprune -r myregistry --progress=tui prune --in rules/delete_untagged_images.json
crprune -r ghcr.io/myorg stats --out stats.json
crprune -r myregistry --progress=plain stats > stats.json
```

| Key | Action |
|-----|--------|
| `q` / `Ctrl-C` | Cancel; wait for active requests to stop before restoring the terminal |
| `l` | Toggle the activity panel |
| `w` | Toggle warnings-only activity |
| `↑` / `↓`, `k` / `j` | Scroll activity |
| `PgUp` / `PgDn`, `Home` / `End` | Scroll by page, jump to oldest, or follow latest activity |
| `?` / `Esc` | Show / close help |

The layout adapts to resizing and honors `NO_COLOR`. The display retains the last 200 log entries and, apart from them, the last 100 warnings (the `w` view), so a busy run does not push warnings out; each entry keeps at most 4 KiB of text. `--progress=plain` logs everything. A scrolled activity view stays put as new activity arrives; `End` follows it again. On closing, the dashboard leaves in scrollback a summary (kept, selected and deleted manifests for `prune`; scanned manifests and unique bytes for `statistics`) and the retained warnings, with the time each was logged (`at=`). A live run counts a repository's selected manifests once the recheck before deleting passes. A dry run counts them without listing them, saying so when it selected any, and its deletion counter stays zero. Byte estimates describe the plan, not confirmed registry garbage collection.

The dashboard uses the controlling terminal, leaving stdout for JSON and stdin for rules; `statistics` writes JSON for stdout once the dashboard has closed, so that a terminal shows it too. Redirected stderr, `TERM=dumb`, and a terminal that cannot be initialized (such as a pseudo-terminal without a controlling terminal) get plain logs; `--progress=tui` reports an error instead.

The dashboard's kept/selected counts describe the plan; deleted counts reflect successful API deletions. Plain logs distinguish `Planned manifests` from `Processed manifests`. If a repository fails partway through deletion, the processed totals still include successful deletions and count everything unconfirmed as remaining. A skipped or denied repository may therefore have been partially pruned. Byte counts remain estimates of referenced blobs, not reclaimed storage.

## Commands

`prune`, `generate` and `top` read stdin when no input file is given, and `-` names stdin or stdout explicitly. When stdin is a terminal, a missing input is an error rather than a silent wait; `--input -` reads the terminal on purpose.

crprune exits with status 0 on success and 1 on failure. `Ctrl-C` or `SIGTERM` cancels a run: crprune says it is stopping, waits for the requests in flight, restores the [locks](#locked-images) it removed for deletions this cuts short, and reports what was done. A second signal exits at once, restoring the terminal if the dashboard owns it; when deleting locked images, it warns that pending lock restores may be abandoned. A run cut short exits with status 130, or 143 for `SIGTERM`.

### `prune`

Deletes manifests (and empty repositories) according to a JSON rule file. By default it is a dry run, logging each manifest it would delete: `Dry-run: deleting manifest`, or, after `Dry-run: deleting repository` for a repository it would delete outright, `Dry-run: deleting manifest with the repository`. The dashboard keeps only counts of that plan, so review it with `--progress=plain`.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--input` | `--in`, `--infile` | stdin | JSON rule file |
| `--dry-run` | `--dryrun` | `true` | Only log what would be deleted; pass `--dry-run=false` (with `=`) to delete |
| `--keep-younger` | `--keepyounger` | `24h` | Grace period [protecting](#protected-manifests) recently updated manifests; a rule duration (`36h`, `7d`, `2w`), not negative |
| `--include-locked` | `--includelocked` | `false` | Delete [locked](#locked-images) manifests too (ACR) |
| `--running` | | | File of running images (from `get_pod_images.sh`) to [protect](#protected-manifests) |

`--running` reads an image list as [`generate`](#generate) does, matching images by tag or by digest. An empty file is refused, and a file naming none of the registry's images draws a warning: with either, no image would count as running.

```sh
# Dry run (default), listing every manifest it would delete
crprune -r myregistry --progress=plain prune --in rules/cleanup_feature_branches.json

# Actual deletion
crprune -r myregistry -v prune --dry-run=false --in rules/delete_untagged_images.json

# Read rules from stdin
cat rules/delete_orphaned_manifests.json | crprune -r myregistry prune --dry-run=false

# Also delete images that have been locked for protection
crprune -r myregistry -v prune --dry-run=false --include-locked --in rules/delete_untagged_images.json

# Delete repositories untouched for two years, except those still running an image
scripts/get_pod_images.sh > images.txt &&
  crprune -r myregistry prune --dry-run=false --in rules/delete_old_repos.json --running images.txt

# Prune the container packages of a GitHub organization
crprune -r ghcr.io/myorg -v prune --dry-run=false --in rules/delete_untagged_images.json
```

#### Protected manifests

These are kept whatever the rules say: manifests updated within the grace period (`--keep-younger`); those without a last-updated time, which every age-based rule would read as infinitely old; running ones (`--running`, logged as `Keeping running image`); and, without `--include-locked`, locked ones. A protected manifest keeps the manifests it references and its referrers (signatures, attestations, SBOMs), and keeps its repository from being deleted outright; under `must_delete_everything`, it keeps the whole repository. A referrer protected by its age alone is the exception: `must_delete_everything` still deletes the rest of its repository, so signed repositories can be bulk-deleted, and a fresh signature outlives its image by at most the grace period.

#### Locked images

A manifest or tag whose `deleteEnabled` or `writeEnabled` attribute is `false` (see [Lock a container image](https://learn.microsoft.com/azure/container-registry/container-registry-image-lock)) is locked, and ACR refuses to delete it. crprune protects locked manifests, and manifests carrying a locked tag, logging each (`Keeping locked manifest; use --include-locked to delete it`). To find locked tags, it lists a repository's tags when the rules would delete one of its tagged manifests, in dry runs too. Under `must_delete_everything`, it also checks the tags of retained signatures and other referrers: a lock on even a freshly pushed signature keeps the entire repository.

**`--include-locked` bypasses this protection.** Just before deleting a locked manifest, crprune enables delete and write on it and its locked tags, each manifest as its turn comes, so those the run never deletes stay locked: the children of an index that could not be deleted, and everything after an interruption. A whole-repository deletion unlocks the repository's locked manifests and tags just before it. If the deletion fails or the run is interrupted, the original `deleteEnabled`/`writeEnabled` values are restored, even after a failed unlock, which may have taken effect; a lock that cannot be restored is logged as a warning. A failed unlock is logged and the deletion attempted anyway; a lock whose state cannot be read is left alone. A dry run changes nothing, logging the manifests it would unlock with `locked=true`, tag locks included.

GHCR has no image locks; there the flag has no effect.

### `statistics` (alias: `stats`)

Writes per-repository size and manifest statistics as JSON. A named output file is replaced atomically during the scan, at most every 5 seconds, and at the end, so a failed write or an interruption leaves the last complete snapshot; a failure before the first result leaves an existing file untouched. Errors after completed repositories still write those results, to stdout too, and exit non-zero. New output files are private (`0600`); existing ones keep their permissions. Output symlinks and non-regular files are refused; use `-` for stdout.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--output` | `-o`, `--out`, `--outfile` | stdout | Output file path |
| `--running` | | | File of running images, read as for `prune`, to count per repository |

```sh
crprune -r myregistry stats --out stats.json
crprune -r ghcr.io/myorg stats --out stats.json --running images.txt
# Sort output by unique bytes:
jq 'sort_by(.unique)' stats.json
# Repositories running no image (with --running), largest first:
jq '[.[] | select(.running == 0)] | sort_by(-.total)' stats.json
```

The output is a JSON array with one object per repository:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Repository name |
| `unique` | int | Bytes counted once per blob (manifest document, config and layers) across the scan |
| `total` | int | Bytes counting every reference, without cross-repository deduplication |
| `shared` | float | `1 - unique/total`: the fraction already counted, in an earlier repository or elsewhere in this one |
| `tagged` | int | Number of tagged manifests |
| `untagged` | int | Number of untagged manifests |
| `count` | int | Number of manifests, including those the registry lists but cannot serve, whose bytes are unknown |
| `newest` | timestamp | Last-updated time of the newest manifest |
| `oldest` | timestamp | Last-updated time of the oldest manifest |
| `running` | int | Manifests (tagged or digest-pinned) matching a keep rule from `--running` (`0` without it) |

A blob several repositories reference counts towards the `unique` bytes of the alphabetically first of them only, so `unique` is not what deleting a repository frees when later repositories share its blobs.

If a listed manifest cannot be downloaded, its known tags, timestamp and running-image status still contribute to the statistics; only its bytes are unknown.

### `generate`

Reads image references, one per line, and writes a rule file keeping only those images, deleting everything else in their repositories. Tag references (`myreg.azurecr.io/repo:tag`, `ghcr.io/myorg/repo:tag`) keep by tag, digest references (`myreg.azurecr.io/repo@sha256:…`) by digest, tagged or not, and a reference with both keeps both, since the digest pins what runs after the tag moved. No tag or digest means `:latest`. References to other registries, or on GHCR other owners, are ignored; host and owner match case-insensitively (`MyReg.azurecr.io/app:v1` is an image of `myreg`). A malformed reference to the registry, or an incomplete or invalid OCI digest, aborts with its line number rather than silently omit a running image. The input need not be sorted or deduplicated: each repository yields one rule. `generate` works offline, without credentials; `--registry` tells it which references are the registry's.

It logs how many non-empty lines it read, matched and ignored, and warns when none names an image of the registry: the rule file is then empty (`[]`), which prunes nothing but usually means a failed inventory or the wrong `--registry`.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--input` | `--in`, `--infile` | stdin | Image list file |
| `--output` | `-o`, `--out`, `--outfile` | stdout | Output file path |

```sh
scripts/get_pod_images.sh > images.txt &&
  crprune -r myregistry generate --in images.txt -o keep-rules.json &&
  crprune -r myregistry --progress=plain prune --in keep-rules.json 2> plan.log
# Review the planned deletions in plan.log, then rerun prune with --dry-run=false.
```

#### Kubernetes image inventory

`scripts/get_pod_images.sh [--scaledjobs] [--pods-only] [context ...]` requires `kubectl` and `jq`, and scans every kubeconfig context unless given some. It collects, deduplicated:

- from pods: normal, init and ephemeral containers, plus fully qualified runtime image digests;
- from the pod templates of Deployments, StatefulSets, DaemonSets, ReplicaSets, ReplicationControllers, Jobs and CronJobs: images that may have no pod right now, of workloads scaled to zero (by KEDA or kube-downscaler, say), CronJobs between runs or suspended, and old ReplicaSets kept for `kubectl rollout undo` (up to each Deployment's `revisionHistoryLimit`; rollback history is included on purpose).

Reading workload templates needs permission to list those resources in all namespaces; without it, the script fails rather than emit a partial inventory. For example:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: image-inventory
rules:
  - apiGroups: [""]
    resources: [pods, replicationcontrollers]
    verbs: [list]
  - apiGroups: [apps]
    resources: [deployments, statefulsets, daemonsets, replicasets]
    verbs: [list]
  - apiGroups: [batch]
    resources: [jobs, cronjobs]
    verbs: [list]
  - apiGroups: [keda.sh]          # only for --scaledjobs
    resources: [scaledjobs]
    verbs: [list]
```

`--pods-only` reads pods only, as earlier versions did, for users who may list pods but not workloads; it misses idle workloads' images, so rules generated from it can delete them. `--scaledjobs` adds KEDA ScaledJobs, failing if they cannot be queried. `KUBECTL_REQUEST_TIMEOUT` overrides the default `60s` request timeout.

The inventory misses images referenced only by manifests not applied to a scanned cluster (Git repositories, Helm charts, deployment pipelines), by other custom resources (such as Argo Rollouts or Knative Services), or by clusters outside the scanned contexts. Add those to the list before running `generate`.

The script buffers its results and emits nothing if any query fails: chain it as above, or with your shell's `pipefail`. It assumes no cluster names and launches no temporary pods. To include historical Mimir/Loki images, set `MIMIR_URL` and/or `LOKI_URL` to full label-values API URLs reachable from the local machine (through port forwarding, say); these optional sources require `curl` and must succeed too.

### `top`

Prints the top repositories of a statistics file (as `statistics` writes it) as an aligned table, sizes human-readable and `shared` as a percentage. It runs locally, needing no registry access or `--registry`. The file may be given as a positional argument instead of `--input`, but not both, and only one.

| Flag | Alias | Default | Description |
|------|-------|---------|-------------|
| `--input` | `--in`, `--infile` | stdin | Statistics JSON file |
| `--sort` | `-s` | `unique` | Sort key: `count`, `name`, `newest`, `oldest`, `running`, `shared`, `tagged`, `total`, `unique`, `untagged` |
| `--top` | `-k`, `-n` | `20` | Number of rows to print (`0` for all) |

Sizes and counts sort largest first, `newest` most recent first, `oldest` oldest first, and `name` alphabetically.

```sh
# Top 20 repositories by unique (deduplicated) size
crprune top stats.json

# Top 10 by total size
crprune top stats.json -s total -k 10

# Straight from a fresh scan
crprune -r myregistry stats | crprune top -s shared
```

```text
NAME        UNIQUE  TOTAL   SHARED  TAGGED  UNTAGGED  COUNT  RUNNING  NEWEST      OLDEST
runner      25 GB   66 GB   62.6%   53      106       159    0        2026-06-15  2025-06-01
gp-profile  11 GB   29 GB   63.5%   75      150       225    0        2026-06-23  2025-11-27
```

## Rule File Format

Rules are a JSON array of repository rules. Each matches repositories by regex and says how to handle their tagged and untagged manifests; only the first rule matching a repository applies to it (see [Rule Evaluation](#rule-evaluation)). Loading the file validates the regexes and rejects:

- unknown fields
- duplicate keys, also those differing only in case (JSON decoding treats them as the same field, so the last would silently win)
- null rule entries
- trailing JSON content
- negative age constraints
- values that would silently match everything: an empty `tag`, `arch`, `os` or `digest` pattern (omit the field instead; in a file that deletes images, `""` is more likely an unset template variable than a deliberate catch-all), `"newest": 0`, a zero `match_newer`, and integer durations below one second

JSON errors give the line and column (`rule file rules.json: line 12, column 24: "keep" must be true or false, not a string`). Rule errors name the rule by its 1-based position and its description (`` rule file rules.json: rule 3 ("prod releases"): tagged rule 2: invalid tag regex "[": error parsing regexp: missing closing ]: `[` ``). An empty array is valid and prunes nothing.

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
| `repo` | string | | Regex matching repository names (on GHCR, package names: `team/app` for `ghcr.io/myorg/team/app`) |
| `ignore_missing_manifests` | bool | `true` | Tolerate missing resources (404) instead of failing |
| `delete_orphaned_manifests` | bool | `false` | Delete manifests whose dependencies are missing |
| `must_delete_everything` | bool | `false` | If any manifest must be kept, keep all (for "delete entire repo" rules; see [Protected manifests](#protected-manifests)) |

### Tagged Rule Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `tag` | string | *(match all)* | Regex matching tag names |
| `arch` | string | *(match all)* | Regex matching the architecture (e.g. `amd64`, `arm64`) |
| `os` | string | *(match all)* | Regex matching the operating system (e.g. `linux`, `windows`) |
| `digest` | string | *(match all)* | Regex matching the manifest digest (e.g. `^sha256:3b1a…$`), for keeping digest-pinned images |
| `newest` | int | | Match only the N newest manifests this rule matches, or all but the N newest when negative |
| `match_newer` | string | | Match manifests newer than this duration (e.g. `24h`, `30d`) |
| `match_older` | string | | Match manifests older than this duration |
| `keep` | bool | `true` | Whether matching manifests are kept or deleted |

### Untagged Rule Fields

Same as tagged rules but without the `tag` field.

### Rule Evaluation

Only the first repository rule whose `repo` regex matches a repository applies to it, so put specific rules before broad ones. Within it, the first tagged or untagged rule matching a manifest decides whether it is kept or deleted; a manifest no rule matches is kept. Durations take Go syntax (`24h`, `168h`), days and weeks (`14d`, `2w`), or integer nanoseconds of at least one second (`86400000000000`); a smaller integer such as `14` is rejected as a likely missing unit (write `"14d"`). They must be non-negative and fit a Go duration. `arch` and `os` in one rule must match the same platform, not one index entry each.

`prune` warns (`Rule never applies`) about rules that can never apply, though such a file still runs:

- a repository rule after one that matches every repository (`.+`, `.*`, `^.*$`, …)
- a literal `^name$` rule for a repository an earlier rule already matches
- tagged or untagged rules after a catch-all in the same list (an entry with no criteria, or with only a `tag` of `.+` or `.*`)

Deleting a manifest deletes all of its tags, so a manifest is judged tag by tag: each tag gets the first tagged rule matching it, and the manifest is deleted only when the rules condemn every one of its tags. A tag that a keep rule matches, or that no rule matches, keeps the manifest. So a feature-branch build later promoted with a release tag survives a feature-branch cleanup, and a build tagged by branch and by commit (`sha-…`) is deleted only once rules condemn both tags. Such a manifest is logged with the tag keeping it (`Keeping manifest: deleting it would also delete a tag the rules keep`).

`arch` and `os` match an image index when any of its platforms matches, so `{"arch": "amd64", "keep": false}` also deletes multi-platform indexes including amd64. To target single-platform images, keep the other platforms first, as `rules/delete_amd64_only_images.json` does with its `arm64` keep rule.

Platforms are resolved through nested indexes and from child images when an index omits its optional platform descriptors. Explicit descriptor constraints are preserved; conflicting reports never combine into an invented OS/architecture pair. These forms are permitted by the [OCI image index specification](https://github.com/opencontainers/image-spec/blob/v1.1.1/image-index.md).

`newest` ranks a manifest against the other manifests **the same rule matches**, not the whole repository. So these rules keep the three most recent release images however many newer feature-branch images sit alongside them:

```json
"tagged": [
  { "tag": "^release-", "newest": 3, "keep": true },
  { "tag": ".+", "keep": false }
]
```

Manifests of equal age rank by digest, so repeated runs over an unchanged repository decide the same way.

`newest` ranks images. Referrers (signatures, attestations, SBOMs) and the manifests an image index references (the platform images and attestations of a multi-platform build), tagged or not, take no ranking slot: they follow the manifest they belong to. So the rules above keep three release builds even when each also tags its platform images (`release-7-amd64`), and `"untagged": [{"newest": 3, "keep": true}, {"keep": false}]` keeps the three newest untagged builds, whole. A platform image that a ranked rule would otherwise match, by any of its tags, is kept exactly when one of the indexes referencing it is kept, and deleted with them otherwise.

## Included Rule Examples

| File | Description |
|------|-------------|
| `rules/cleanup_feature_branches.json` | Deletes feature branch/PR images older than 14 days, and untagged manifests older than 24 hours |
| `rules/delete_untagged_images.json` | Deletes untagged manifests older than 24 hours |
| `rules/delete_orphaned_manifests.json` | Deletes manifests with missing dependencies |
| `rules/delete_old_repos.json` | Deletes all content from repos where everything is older than 730 days |
| `rules/delete_amd64_only_images.json` | Deletes repos that contain only amd64 images (no arm64) |

Every example applies to all repositories. Those deleting untagged manifests also delete digest-pinned images whose tag has since moved to a newer build; protect running ones with `--running`. `cleanup_feature_branches.json` recognizes one team's date-stamped build tags: `YYYYMMDD-br`, and `YYYYMMDD.N-<kind>.<name>` where `<kind>` is `br`, `pr`, `ft`, `local` or `db` (for example `20260930.2-pr.1234`). It keeps every other tag, so adapt its `tag` regex to your own branch tags, such as `^(pr|feature)-`.

## Behaviour Notes

- A repository the rules leave no manifest in is deleted entirely (respecting `--dry-run`). A repository listing no manifests at all is left alone: ACR and GHCR keep no empty repositories, so that is an anomaly, and deleting it would free nothing. If a listed manifest cannot be downloaded, its entire repository is skipped, since an unavailable index could reference any of its other manifests; the other repositories are still processed.
- Deletions remove an index before the manifests it references, and an image before its referrers: a signature left behind is harmless garbage, while an image that lost its signature can fail admission. A failed deletion leaves everything depending on it intact, such as the platform images of an index or the signatures of an image that could not be deleted. Independent manifests are still deleted, and every failure is reported: a repository whose failures are all permission errors is reported as denied and the run goes on; any other failure stops the run. A referrer left behind by a failure or interruption after its subject was deleted goes on the next run if it has a `subject` field, and otherwise becomes an ordinary tagged manifest (see below).
- Referrers, manifests with a `subject` field (signatures, attestations, SBOMs) or naming their subject by tag, follow their subject instead of matching rules: kept exactly as long as it is, deleted with it or when it is already gone, unless [protected](#protected-manifests) themselves. Naming by tag covers cosign's default scheme (`sha256-<digest hex>.sig`, `.att`, `.sbom`) and the OCI referrers tag schema's index of referrers (`sha256-<digest hex>`), when every tag of the manifest names the same subject and that subject is in the same repository. Otherwise, as for a signature whose image is gone, or one in a separate `COSIGN_REPOSITORY`, whose images live elsewhere, the manifest is an ordinary tagged one, which only a rule matching its tag deletes. To clean up dangling signatures, add a tagged rule like this one, with an age limit, to the rule for the images' repository, never to a `COSIGN_REPOSITORY`'s:

  ```json
  { "tag": "^sha(256|512)-[0-9a-f]+(\\.(sig|att|sbom))?$", "match_older": "7d", "keep": false }
  ```

- Listings are a point-in-time snapshot, so before deleting anything from a repository, manifest by manifest or outright, crprune lists it again. New or missing manifests, and changed tags, timestamps or locks (tag locks included, when loaded), mean it changed during inspection: it is skipped with nothing deleted, the other repositories are still pruned, and the run exits non-zero naming it; rerun to prune it. This also catches a manifest a concurrent deletion hid from GHCR's page-numbered listing, whose platform images would otherwise look unreferenced. A manifest listed twice during pagination skips the repository the same way, rather than a decision from conflicting attributes; a repository someone else deleted meanwhile is skipped as well. The recheck narrows the race with concurrent writers, but registries offer no atomic compare-and-delete: avoid pushes and retagging during a prune, especially while unlocking protected images.
- A manifest in a format crprune cannot decode (Docker schema 1, pre-release OCI artifact manifests) hides what it references, so its repository is skipped with a warning; the other repositories are still processed, and the run exits non-zero naming it. `statistics` skips such repositories, and those that change during their scan, the same way. A signed schema 1 manifest is reported as unsupported, not as a digest mismatch. Invalid descriptor digests and negative blob sizes stop the run.
- The per-repository byte counts `prune` logs deduplicate blobs within the repository only; the registry's garbage collector may not free layers shared with other repositories.
- A manifest is orphaned when something it references is missing, an index child or a `subject` alike; the flag propagates up to the indexes referencing it and down to its children. A child still reachable through a healthy index or its own tag is not orphaned by a broken sibling.
- When every rule targets a literal repository name (`^name$`), only those repositories are fetched, without listing the registry. A pattern with an active metacharacter is no literal name: `^my.repo$` matches `myXrepo` too, so it is resolved by listing the catalog. `^my\.repo$` (what `generate` emits) addresses a repository with a dot in its name directly.
- Cached manifests are stored under `<cache>/<canonical-registry>/` (for example `myreg.azurecr.io` or `ghcr.io/myorg`), created when a command starts (an unusable path is an error), and removed when deleted from the registry. Image configs read for `arch`/`os` rules on GHCR are cached alongside them. Cached content is verified against its digest on every read, and reads are bounded to 16 MiB. Writes replace files atomically but are not flushed to disk, since a damaged file is simply downloaded again; the first failure to write is logged as a warning. New cache directories and files are private.

### Azure Container Registry

- HTTP requests have a two-minute timeout, including reading the response body. Truncated manifest downloads are retried before content is parsed or cached. Redirects cannot change a deletion into a read or forward a mutation to another origin. Read redirects drop authentication credentials when the origin changes, and HTTPS requests never redirect to HTTP. Repeated pagination links fail instead of looping indefinitely.
- Throttled requests (429) and failing ones (408, 500, 502, 503, 504, timeouts, connections refused, reset or cut short) are retried up to 10 times; a host name that does not resolve or a certificate that does not verify fails at once. crprune waits as long as the registry asks (`Retry-After`, up to 3 minutes), or backs off exponentially from 2 seconds to 3 minutes, riding out about a quarter of an hour of throttling per request. Each retry is logged as a warning and counted down in the dashboard.
- ACR access tokens are scoped to one repository and action. crprune keeps separate clients for listing, attribute updates, and manifest content and deletion. The first request of each action on a repository fetches the token and the others reuse it, so a run exchanges only a few tokens per repository.

### GitHub Container Registry

- Repositories are the owner's container packages, and manifests their package versions. ghcr.io's registry API can neither list untagged manifests nor delete, so crprune lists and deletes through the [GitHub REST API](https://docs.github.com/en/rest/packages/packages) and downloads manifest content from ghcr.io. Deleting a manifest deletes its package version, with every tag pointing at it.
- A multi-platform push stores its platform images and attestations as separate, *untagged* package versions. They are kept as dependencies of the index referencing them, so cleaning up untagged versions does not break multi-platform images.
- GHCR refuses to delete a package's last tagged version ("You must delete the package instead"). When the rules would keep only untagged manifests of a package (a digest-pinned running image, say), crprune also keeps the newest tagged image, with its dependencies and signatures, and logs a warning. When the rules keep nothing, the whole package is deleted. Deleted packages and versions can be [restored](https://docs.github.com/en/packages/learn-github-packages/deleting-and-restoring-a-package) for 30 days.
- GHCR reports no image platforms. Images an index references take theirs from the index; for single-platform images, rules using `arch` or `os` download the image config (once per distinct config, cached). Other rules download nothing extra.
- The last-updated time is the package version's `updated_at`.
- GitHub rate limits are honoured. A throttled request pauses all of crprune's GitHub requests, so that parallel workers do not keep hitting the limit, for as long as GitHub asks (`Retry-After`, or until the limit resets, by GitHub's `Date` header rather than the local clock). A secondary limit giving no wait time is waited out for a minute, doubling while it persists until a request gets through. Throttled requests are retried, with a warning, for up to two hours per request, riding out GitHub's hourly limits during a large cleanup. Server and network errors are retried a few times with exponential backoff.
- Workers recheck a shared pause after waiting, so a later response extending the rate limit delays them too. Permanent DNS, certificate and redirect-policy failures fail immediately. Truncated success responses are retried, and response documents are bounded to 16 MiB. GHCR uses the same credential and method protections on redirects as ACR, including blob-storage redirects.
- GitHub answers 404 both for a package that does not exist and for a private package the token may not read, so a literally named package (`^name$`) the token cannot see is reported missing rather than denied. If a package you expect is reported missing, check the token's access to it.
- GitHub does not let a public package be deleted, in whole or in part, once one of its versions has been downloaded more than 5,000 times; crprune reports GitHub's error for such a package.

## Package Layout

| Package | Responsibility |
|---------|----------------|
| `cmd/crprune` | CLI wiring (flags, commands, I/O, credentials, signals) |
| `internal/rules` | JSON rule format, validation/compilation, rule generation from image lists |
| `internal/registry` | Registry-neutral access: `--registry` parsing, manifest model, parallel manifest download with digest verification, on-disk cache, platform resolution, deletion; the `Backend` interface a registry API implements |
| `internal/registry/acr` | `Backend` for Azure Container Registry, on the `azcontainerregistry` data-plane client |
| `internal/registry/ghcr` | `Backend` for GitHub Container Registry: GitHub REST packages API, ghcr.io registry token exchange, rate-limit-aware retries |
| `internal/registry/registrytest` | In-memory `Backend` and log recorder for tests |
| `internal/pruner` | Rule evaluation, orphan detection, keep/delete decisions, statistics |
| `internal/progress` | Concurrent progress tracking, bounded logs, and the lightweight terminal dashboard |
| `internal/imageref` | Shared repository and image-reference validation |
| `internal/jsonpos` | Line and column of JSON decoding errors |
| `internal/fileio` | Atomic file snapshots |

## Development and validation

```sh
make build       # local binary, version from git
make test        # Go tests plus the mocked script tests (the inventory tests require jq)
make test-race   # Go race detector plus the script tests
make coverage    # race-enabled coverage.out and function coverage
make vet
make lint        # pinned golangci-lint, installed into ./bin on first use
make vuln        # vulnerability scan, built with the active Go toolchain
make dist-all    # Linux amd64/arm64 archives and checksums
```

The tests use in-memory registries and HTTP test servers; they need no cloud credentials and delete no real packages. They cover rule validation, dependency and referrer lifecycles, nested indexes and omitted platforms, pagination loops, extended throttling, credential redirects, partial deletion summaries, cancellation and signals, atomic output, CLI workflows, and terminal simulation at multiple sizes. Fuzz targets exercise rule and manifest parsing and image-reference validation. For an extended local run:

```sh
go test ./internal/rules -fuzz=FuzzParseAndCompile -fuzztime=30s
go test ./internal/imageref -fuzz=FuzzSplit -fuzztime=30s
go test ./internal/registry -fuzz=FuzzParseManifest -fuzztime=30s
go test ./internal/pruner -run '^$' -bench BenchmarkCountRunning -benchmem
go test ./internal/registry -run '^$' -bench BenchmarkIndexPlatforms -benchmem
make vuln
```

CI also runs `make vuln` with its Go 1.27 toolchain. The target builds the checker with the active Go version, avoiding incompatibility with a previously installed checker built for an older Go release.

Every CI run keeps the `make dist-all` archives and checksums of its commit as a workflow artifact named `crprune-<commit>`. Every push to `main` publishes a GitHub release with them and `download.sh`.

## Similar Work

- https://github.com/Azure/acr-cli

crprune adds declarative retention rules, dependency-aware cleanup, and support for both ACR and GHCR.

## License

[MIT](LICENSE)
