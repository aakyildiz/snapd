# Reproductions of the findings

Every finding in `STATUS.md` (1-13) is reproduced twice: by a deterministic Go
unit test, and by a scenario on a real snapd in an Incus container, run both
locally and on Antithesis.

## Unit tests (local, deterministic)
Behind the build tag `antithesisrepro`; each test asserts the correct
behaviour, so it fails while the bug exists.

```
GODEBUG=rsa1024min=0 go test -count=1 -tags antithesisrepro <pkg> -check.f AntithesisFinding -check.v
go test -race -count=1 -tags antithesisrepro ./tests/lib/fakestore/store -check.f AntithesisFinding12
```
Packages: `overlord/state`, `overlord/snapstate`, `overlord/snapstate/backend`,
`systemd`, `osutil`, `snap/squashfs`, `daemon`, `tests/lib/fakestore/store`.
Result (2026-11-25): all fail as expected, with assertion mismatches (no panics);
the one control test for finding 3 passes. Finding 12's test fails under
`-race` on the store before `d52460e` and passes with both store fixes
(`d52460e`, `53959a2`; the second fixes a related race this test found:
requests mocked and restored `snap.SanitizePlugsSlots`, a global, so one
request could restore the panicking default while another read snap info).

## System scenarios (`snapd-incus/driver/repro.go`)
They run next to the main workload, in the same driver, against a second host
`snapd2` that is excluded from Antithesis faults so each scenario is exact.
The main workload on `snapd1` keeps every fault. Tools:
- fault points compiled into snapd with snapd's own `faultinject` build tag
  (commit `cf6e2b0`), enabled per scenario through the agent's `/fault`
  endpoint (`SNAPD_FAULT_INJECT`): `panic` (a crash at an exact point),
  `pause`, `pause-after-crash`, and `retry-after-crash` (after a crash, the
  re-run task waits so an abort can land first, as after a real restart);
- a `systemctl` shim (`/usr/local/sbin/systemctl` in the container) that fails
  calls matching a pattern (finding 3);
- a store cut-off (finding 11);
- the fake store as it was before its fixes, on `store:11030` (finding 12).

Each finding N has `repro N: the scenario reaches its check` (Reachable),
`repro N: <correct behaviour>` (Always; fails while the bug exists) and
`repro N: finding N reproduced` (Sometimes).

| # | Scenario | Antithesis run 11: reproduced / checked | Example moment (input_hash, vtime) |
|---|---|---|---|
| 1 | abort a sideload right after it is accepted | 264 / 264 | 8525336808058684702, 13.08 |
| 2 | sideload over a disabled snap | 266 / 266 | 4839308297911480083, 14.77 |
| 3 | sideload while systemctl daemon-reload and the snap's unit calls fail | 100 / 100 | -1214518278298881797, 25.85 |
| 4 | abort a remove after clear-snap | 22 / 22 | -5333981858622033829, 34.85 |
| 5 | abort a remove after unlink-snap; common dir already gone | 14 / 14 | -5817456486769187023, 46.04 |
| 6 | crash in discard-snap after file removal, abort before the re-run | 18 / 18 | 304332272915897233, 48.51 |
| 7 | crash after discard-snap saved its work, before Done | 19 / 19 | -995955499356238863, 50.71 |
| 8 | crash in mount-snap after the upload is removed | 23 / 23 | 8062634630885065758, 52.08 |
| 9 | sideload the same snap again over the leaked, mounted revision | 21 / 21 | 8736356715001729433, 52.81 |
| 10 | crash in discard-snap of the last revision; ensure rewrites the unit in the re-run's unlocked window | 24 / 24 | -4294013433455137123, 62.41 |
| 11 | store cut off just before check-rerefresh asks it | 36 / 36 | 195713744809831127, 115.54 |
| 12 | 64 concurrent requests to the unfixed store, up to 5 bursts | 11 / 80 | 8225538073537461117, 116.63 |
| 13 | crash in link-snap after the symlink, abort before the re-run | 50 / 50 | -7921424676750188425, 137.31 |

Run 11: `0f467b9c74a6af9fbdf68befa89b61cd-68-0` (12 CH cap, commit `64fc1b1`).
Locally (`scripts/local-run.sh`), every scenario reproduced in at least one
round before the launch.

Notes from building the scenarios:
- 1: no fault needed. `prerequisites` finishes and `prepare-snap` waits a
  second or more, so an abort sent right after the sideload is accepted lands
  in between most of the time; attempts where it does not are repeated.
- 10: the ensure must run after discard-snap's `RemoveSnapFiles` and before it
  relocks the state; running it before (the first try) leaves nothing behind,
  because the re-run removes the unit it wrote.
- 11: `check-rerefresh` starts early and retries until the refreshes finish, so
  the fault point is just before it asks the store, not at its start.
- 12: the store only races while it has files it has not cached; a burst
  without new files never crashes it.
- Main workload in the same run: its failing properties are the known findings
  (2, 1 and the leak checks).
