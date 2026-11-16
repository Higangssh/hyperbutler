# 0011. A default build cache limit

- Status: proposed
- Date: 2026-11-16
- Requirements: REQ-DISK-006, REQ-UX-007

## Context

REQ-DISK-006 asks that, with default settings, BuildKit's build cache stays at or under
10 GB (or a configured limit). dockerd's BuildKit collects garbage by default, but its
default policy scales with the disk: with no `builder` settings, dockerd (moby v28,
`builder/builder-next/worker/gc.go`) lets the cache use up to 80 % of the data disk
(with 10 % reserved and 20 % to keep free), so on hyperbutler's default 64 GiB data disk
the cache may grow to tens of GB before anything is pruned. The data disk is a VHDX on
the user's system drive, so that space is gone from Windows until a trim shrinks the
file.

The limit has to reach dockerd. dockerd reads its builder settings only from
daemon.json (`builder.gc`), and hyperbutler passes the user's daemon.json through
unchanged (REQ-UX-007: "reaches the guest's dockerd byte for byte"). The guest starts
dockerd with at most that one file.

## Options

1. **Add the policy to the delivered file when the user's file does not configure
   `builder.gc`** (`daemoncfg.WithDefaults`): `"builder":{"gc":{...}}` before the closing
   brace when there is no `builder` key, or `"gc":{...}` inside the user's `builder` object
   when it sets something else (entitlements, history); with no user file, a file with
   only that key. A file that sets `builder.gc` (in any case dockerd's decoder folds to it)
   goes through byte for byte, so the user's own policy replaces the default entirely. The
   file on disk, its digest in `status`, and the applied copy stay the user's bytes; only
   what is sent to the guest changes. Cost: REQ-UX-007's "byte for byte" gets an exception.
2. **Prune from the guest agent on a timer** (`POST /build/prune` with a max used space,
   API 1.48): daemon.json stays untouched. But BuildKit's own collection runs after
   builds when the cache is over the limit; a timer either runs often (work while idle) or
   lets the cache overshoot between runs, and it has to parse the user's file in the guest
   to stay out of the way of their own policy.
3. **A default only when there is no user file.** Keeps REQ-UX-007 literally, but anyone
   who adds `insecure-registries` silently loses the cache limit.
4. **Do nothing; document `builder.gc`.** Leaves REQ-DISK-006 unmet by default.

## Decision

Option 1. It uses dockerd's own mechanism, so collection happens when BuildKit decides,
not on a timer, and a user who configures the collector gets exactly their file.

The policy is `{"enabled":true,"defaultReservedSpace":"2GB","defaultMaxUsedSpace":"8GB",
"defaultMinFreeSpace":"5GB"}`. dockerd parses the sizes with `units.RAMInBytes`, so "GB"
is GiB. dockerd fills in its disk-based defaults only when all three sizes are zero
(`DefaultGCPolicy`), so setting the cap alone would also drop the min-free rule; the
three together mean: prune above 8 GiB (8.6 GB, under the requirement's 10 GB with room
for what a build adds before the next collection), and while the data disk has less than
5 GiB free prune down towards a 2 GiB floor. The cap is `defaultMaxUsedSpace`;
`defaultKeepStorage` (what Docker Desktop sets) is now the reserved space, and alone it
still acts as a cap (BuildKit prunes down to the larger of reserved and what the other
limits allow, `cache/manager.go`), but together with a cap it is the floor.

Check keeps user files `DefaultsRoom` (256) bytes under the guest's 64 KiB limit, so the
delivered file always fits.

The guest image ships with the engine (it is pinned per release), so the dockerd that
gets the policy is the one this was checked against (29.9.3). The engine does not retry
the user's file without the policy: a dockerd that refuses it would refuse it every time,
and retrying the user's file costs another dockerd start (up to 90 s) after a rollback.
If the guest refuses even the policy alone, the engine falls back to no file and the
status names the build cache limit, not the user's file.

## Adversarial review

The review (2026-11-16) read moby v28.5.2 and BuildKit v0.34.2 and tried to break the
first version.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | High | A user file within 64 bytes of the 64 KiB limit passed Check but was over the guest's limit with the policy added, and delivery failed outright. | **Fixed:** Check keeps user files DefaultsRoom under the limit; a test at the limit and the fuzz target check that the delivered file fits. |
| 2 | Med | An older dockerd does not refuse `defaultMaxUsedSpace`; it ignores unknown builder sub-keys, so the fallback the first draft described never happens and the cache is unbounded without a signal. | **Accepted, reworded:** the guest's dockerd is pinned with the engine. A dockerd bump must check `docker buildx inspect` (release checklist). |
| 3 | Med | When the policy is refused, the user's valid file was dropped and the status blamed it. | **Partly fixed:** the status now names the build cache limit when the policy alone was refused. The user's file is not retried without it (see Decision). |
| 4 | Med | Any `builder` key, such as `entitlements` alone, removed the limit. | **Fixed:** the policy goes in at `builder.gc`: inside the user's `builder` object when it has no `gc`. |
| 5 | Med | The cap alone dropped the min-free rule; the first draft's reason for not using keep storage was wrong. | **Fixed:** reserved, max used and min free together; the text above cites BuildKit's keep-bytes rule. |
| 6 | Med | `status`, `config` help and docs said "dockerd defaults" and "byte for byte". | **Fixed:** they name the limit; usage.md documents it and how to override it. |
| 7 | Low | The cap counts unshared records only, and collection runs at most once a minute after a build, so `docker system df` can exceed 8 GiB and a fast compose cycle can overshoot. | **Accepted:** `bench/buildcache` must measure `docker buildx du` and record the peak; not built yet. |
| 8 | Low | After a boot rollback of the user's file the engine now also tries the policy alone, which is a second dockerd start. | **Accepted:** stated here; the earlier path ended with no file and no limit. |
| 9 | Low | `{"Builder":{}}` is refused by `dockerd --validate` anyway; tests encode outcomes dockerd never reaches. | **Commented** in the tests. |
| 10 | Low | A VM that outlives the engine (an upgrade without stopping it) gets the new file at the first connection, and the guest restarts dockerd once. | **Accepted:** noted for the release notes. |
| 11 | Low | The sizes are not checked by `dockerd --validate`; a bad constant would fail dockerd at start. | **Mitigated:** a test pins the constants; the guest's boot rollback catches a bad one. |
| 12 | Low | A test name still said "byte for byte". | **Fixed.** |

## Consequences

- REQ-UX-007 and the docs say the file is delivered byte for byte except for the build
  cache policy, which hyperbutler adds when the file does not configure `builder.gc`.
- The `buildkit.gcKeepStorage` setting stays reserved; daemon.json's `builder.gc` is the
  way to change the limit. If users ask for a setting, it would map to
  `defaultMaxUsedSpace`, not to keep storage.
- Not measured yet: `bench/buildcache` (30 `docker compose build` cycles) does not exist.
  The policy dockerd runs with is checked with `docker buildx inspect`.
- Checked 2026-11-16 on Windows 11 Home with the v0.2.0 guest image (dockerd 29.9.2,
  BuildKit v0.34.0), an engine built from this change in its own home: with no
  daemon.json, `docker buildx inspect` lists rules 1 to 3 with "Reserved Space: 2GiB, Max
  Used Space: 8GiB, Min Free Space: 5GiB"; with a daemon.json that sets only
  `builder.entitlements` and `debug`, `config daemon apply` restarted dockerd in 2.0 s and
  the same rules were in effect.
