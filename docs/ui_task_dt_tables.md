Read `docs/ui_session_brief.md` first — it is your role, the design bar, and the
constraints that cause silent breakage. Then execute the task below.

This is a work order, not a consultation. Do not audit the app, do not produce a
findings list, do not ask which option I prefer. Make the changes, then show me
what you changed.

---

## Task: consolidate every data table onto the canonical `.dt`

`.dt` already exists in `cmd/web/static/style.css:5478` and almost nothing uses
it. Nine other table classes do the same job with their own CSS. The plan
(`docs/ui_overhaul_plan.md` item #6) calls this the single highest-leverage
change left, because it reskins four pages at once.

Current state, measured just now:

| class | templates | CSS lines |
|---|---|---|
| `legend-table` | dashboard, onchain | 9 |
| `setups-table` | fundamentals, setups, tw | 14 |
| `fund-table` | fundamentals, tw | 19 |
| `mkt-table` | today | 17 |
| `onchain-table` | onchain | 6 |
| `slice-table` | fundamentals, setups, tw | 3 |
| `dump-table` | onchain | 2 |
| `ops-oi` | ops | 0 |
| `holder-table` | onchain | 0 |

Do this:

1. Make `.dt` good enough to replace all nine. It already has sticky headers,
   tabular-nums, row hover and a bottom-border rhythm. Add the modifiers the
   variants actually need — `.dt--compact`, `.dt--numeric`, whatever the real
   markup demands — as modifiers on `.dt`, not as new standalone classes.
2. Migrate the templates to `.dt` (+ modifier).
3. Delete the dead CSS for the nine, including the `.setups-table` alias that
   currently rides along on the `.dt` rules.
4. `legend-table` may genuinely not be a data table. If so, say why in one line
   and leave it — do not force it.

### Acceptance — I will run these

```
grep -rcE 'setups-table|fund-table|mkt-table|onchain-table|slice-table|dump-table|ops-oi|holder-table' cmd/web/templates/ cmd/web/static/style.css
```
must be 0 everywhere (legend-table excepted if you justified it).

```
go run ./cmd/web
```
then `/setups`, `/fundamentals`, `/tw`, `/onchain`, `/today`, `/ops`, `/` must
each render with no visual regression — same information, same density or
better, at **390px and 1920px**, in **both themes**.

### Non-negotiable while you do it

- Every `color-mix()` gets a solid rgba fallback line immediately before it.
  iOS Safari < 16.4 renders no background at all otherwise.
- Chart data-viz colours stay un-themed. Do not touch `chart.html` in this task.
- Tokens only. No new hardcoded hex.
- `go build ./...` passes.

### Report back

A table of files touched with net line delta, the `grep` output above, and one
line per deliberate visual change. No prose summary of what tables are.

---

After I approve this, next in order — do not start them now:
2. Type scale: 52 distinct `font-size` values in style.css collapse onto the 7
   existing `--fs-*` tokens.
3. Unify `.badge` / `.tile` / `.banner` primitives.
