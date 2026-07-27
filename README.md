# layby

**[layby.appwrite.network](https://layby.appwrite.network)**

Declarative, disposable sandbox environments defined by a `mise.toml`, provisioned on
infrastructure you choose, and destroyed on a timer.

Think Cursor's cloud agents, but you pick the machine, you own the image, and the whole
thing runs on your own metal if you want it to.

> **Status:** early. Two drivers ship, and both are exercised end to end — provision,
> exec, env, exit codes and teardown. `local` runs on Docker; `digitalocean` is verified
> on real infrastructure.
>
> Nothing else is written yet. Candidate providers are listed under
> [Provider shortlist](#provider-shortlist); the CLI says a driver is missing rather than
> failing halfway through a provision.

## Why

Cloud agent products give you someone else's environment on someone else's machine. This
inverts that: the environment is a file in your repo, and the machine is whichever provider
you point at.

The load-bearing idea is that [mise](https://mise.jdx.dev/) removes the need for a Dockerfile
per stack. One universal base image plus a `[tools]` table describes any toolchain, and the
same file that configures your laptop configures the sandbox.

## Install

```sh
go build -o bin/layby ./cmd/layby
```

Requires Go 1.26+ and a running Docker daemon for the `local` driver.

## A blueprint is just a mise.toml

`[tools]` and `[env]` are standard mise and work with a plain `mise install`. The `[sandbox]`
table is this tool's namespaced extension, which mise ignores.

```toml
[tools]
node = "22"
jq = "1.7.1"

[env]
NODE_ENV = "development"

[sandbox]
provider = "local"     # local | digitalocean
ttl = "1h"             # sandboxes destroy themselves
idle_timeout = "20m"

[sandbox.hooks]
setup = ["npm ci"]
verify = ["npm test"]
```

## Usage

```
layby up      [-f mise.toml] [--ttl 1h] [--rebuild]  provision and wait until ready
layby ls                                             list sandboxes with age and TTL remaining
layby run     <id> -- <command...>                   run a command, passthrough exit code
layby shell   <id>                                   interactive shell
layby down    <id> | --all | --expired               destroy sandboxes
layby doctor  [-f mise.toml]                         reconcile provider state, report orphans
layby explain [-f mise.toml]                         show resolved blueprint and Dockerfile
layby image   tag | context <dir>                    inspect or materialise the build definition
```

`layby help <command>` prints the full flag list. Long flags also accept the older
single-dash spelling (`-all`, `-ttl 1h`), so scripts written against earlier versions
keep working.

`layby up` prints only the sandbox identifier to stdout, so it composes:

```sh
ID=$(layby up)
layby run "$ID" -- npm test || echo "tests failed with $?"
layby down "$ID"
```

## Design notes

**The image is the cache.** Rather than running `mise install` at boot — minutes on every
start, and real money on a metered provider — the toolchain is baked into a container image
at build time. Booting is then a pull. The expensive install runs once per unique build
definition, on whatever builder is cheapest.

**The image tag hashes the whole build definition,** not just the toolchain. Keying on the
toolchain alone means a corrected Dockerfile silently reuses a stale image, and the fix
appears to do nothing. The tag deliberately excludes the blueprint's file path so the same
toolchain hashes identically on every machine and a prebuilt image can be shared.

**The blueprint is installed as mise's global config**, not a directory-scoped one. Shims
resolve a tool version by walking up from the current directory, so a project-scoped config
leaves tools unresolvable anywhere else — including the repository an agent works in.

**`PATH` is exported twice, on purpose.** Non-interactive `exec` picks it up from the image's
`ENV`; a login shell sources `/etc/profile`, which resets `PATH`, so `/etc/profile.d/10-layby.sh`
re-exports it. Without both, `layby run` and `layby shell` disagree about whether `node` exists.

**The provider is the source of truth, not local state.** `~/.layby/state.json` is a cache. A
running sandbox with no local record is an orphan, and `layby doctor` reports it — a lost laptop
should cost a reconciliation pass, not a leaked instance. `layby down` resolves identifiers
against the provider too, so the orphan it reports is one you can actually destroy.

**Exit codes pass through.** An agent has to be able to run `layby run $ID -- pytest` and branch
on `$?`. Human-readable progress goes to stderr; stdout stays clean.

## Measured

Blueprint: `node = "22"` + `jq = "1.7.1"`. Published image is **154 MB**.

| Operation | Time |
| --- | --- |
| Cold build, local Docker on Apple Silicon (empty layer cache) | 3m31s |
| Rebuild, base layers cached | 1m47s |
| **Cold build on GitHub Actions, linux/amd64, pushed to GHCR** | **46s** |
| **Warm provision, local driver (image present)** | **485ms** |
| **Cold boot on DigitalOcean — instance create to usable sandbox** | **2m 45s** |

Building in CI rather than locally is not a small optimisation — it is 4.6x faster than the
same cold build on an M-series laptop, and it is free.

The DigitalOcean figure is the honest end-to-end cost of a sandbox on real
infrastructure: instance provision (~45s), boot, then cloud-init pulling the
154 MB image and starting the container. Measured from a clean slate in `blr1`
on `s-1vcpu-1gb`, which costs $0.00893/hr — the sandbox above cost well under
a cent. Once running, exec is instant: connections are multiplexed, so a burst
of twelve commands reuses one TCP session.

The 485ms warm number is a floor, not a forecast. The local driver skips both the registry
pull and VM scheduling, which are exactly what will dominate on a real provider.

## Providers

| Provider | Billing | Boot | Snapshot | Per-sandbox keys |
| --- | --- | --- | --- | --- |
| `local` | free | ~400ms | no | n/a |
| `digitalocean` | hourly, from $0.0089/hr | ~2m45s to usable | yes | yes |

DigitalOcean is the default for anything that has to outlive your laptop: hourly
billing makes a per-task sandbox cost a fraction of a cent, tags are first-class so
reconciliation never depends on a naming convention, and keys attach per instance.
`local` stays the fast path for anything that fits on the machine in front of you.

**One deliberate gap on DigitalOcean:** there is no true in-sandbox
self-destruct. Doing it properly means writing an API token onto a box that runs
untrusted agent code, and a full-scope token is worse than an occasional orphan.
cloud-init powers the instance off at expiry, but a stopped instance still
bills for its disk — `layby down --expired` is what actually stops the meter.

## Provider shortlist

None of these are written yet. A provider makes the list when it has a real
create-a-machine primitive, billing granular enough that a ten-minute sandbox costs
ten minutes, and keys that attach per instance rather than per account. Ordered by how
well they clear that bar.

| Provider | Create primitive | Billing granularity | The catch |
| --- | --- | --- | --- |
| Fly.io | Machines API | per-second while running | a stopped machine still bills its rootfs — the same trap as a stopped instance |
| AWS EC2 | `RunInstances` | per-second, 60s minimum | tags and per-instance keys work like DigitalOcean's; the API surface is the cost |
| Google Compute Engine | `instances.insert` | per-second, 60s minimum | same shape as EC2, same weight |
| Linode (Akamai) | `POST /linode/instances` | hourly, billed only while the instance exists | no partial-hour minimum, but no per-second either |
| Vultr | `POST /instances` | hourly | hourly like DigitalOcean, without the snapshot story |
| Hetzner Cloud | `POST /servers` | hourly, partial hours rounded up | cheapest per hour on the list, but a three-minute sandbox costs a full one |

Billing models are from each provider's public documentation as of July 2026, not from
a run — no sandbox has booted on any of them. The measured table above exists precisely
because published numbers and observed ones disagree.

## Roadmap

- [x] Blueprint parsing, tool hashing, image build pipeline
- [x] `local` Docker driver, full lifecycle
- [x] TTL tracking, orphan reconciliation via `layby doctor`
- [x] `digitalocean` driver — instance from a prebuilt GHCR image, hourly billing
- [ ] Snapshot after pull, to cut cold boot to roughly instance-create time
- [ ] In-sandbox self-destruct daemon so a closed laptop cannot leak a paid instance
- [ ] Spend ceiling — refuse `up` past a configured budget
- [ ] `ssh` driver — point at a box you already own
- [ ] MCP server exposing sandboxes as agent tools
- [ ] Git-based egress: agent works in the sandbox, pushes a branch, opens a PR
