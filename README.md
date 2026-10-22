# heating-export

An occasional, read-only Go CLI for exporting Balance meter history and SALUS Premium Lite thermostat change events. No UI, daemon, scheduler, database, control commands or LLM integration. Uses Go's standard library, including embedded timezone data. Go 1.24+.

## Build and configure

```sh
go build -o heating-export .
cp config.example.json config.json
chmod 600 config.json
${EDITOR:-vi} config.json
```

Replace the two sets of credential placeholders. Tokens remain in memory. To disable a source, remove its section or set it to `null`; alternatively use `--source balance` or `--source salus`, which validates and contacts only that source. Empty selection arrays mean **all authorized devices**; populated arrays restrict export. Discovery always lists the authorized devices, independent of selection filters.

Balance's statically traced base is **HTTP**, not HTTPS. Its credentials and Bearer tokens would travel in cleartext. `allow_insecure_http` defaults to false and blocks that connection before login. Use a trusted secure transport/base URL if available, or explicitly set this field to true after accepting that transport exposure. HTTPS support at the vendor endpoint has not been established; changing the scheme alone is not a tested fix. SALUS requires HTTPS.

The known config filenames and `exports/` are Git-ignored. Every export also contains its own `.gitignore` so exports under other directory names are ignored. Output directories/files have modes 0700/0600. Keep configuration under an ignored `config*.json` filename; credentials are never command-line arguments or exported configuration. Do not commit another arbitrarily named credential file.

## Discover and export

```sh
./heating-export discover --config ./config.json
./heating-export discover --config ./config.json --source salus

./heating-export export --config ./config.json \
  --from 2026-10-01 --to 2026-10-19 --out ./exports/october

./heating-export export --config ./config.json --source balance \
  --from 2026-10-01 --to 2026-10-19 --out ./exports/balance-october

./heating-export export --config ./config.json --source salus \
  --from 2026-10-01 --to 2026-10-19 --out ./exports/salus-october
```

`--to` is **inclusive**. Calendar dates use `timezone`, default `Europe/Chisinau`. The internal range is `[first local midnight, midnight after --to)`. Calendar stepping and UTC conversion preserve 23/25-hour DST days. UTC timestamps use `Z`; local timestamps include their actual offset. SALUS requests use explicit UTC instants with a one-second overlap before each window to protect against an exclusive server start; local filtering retains the logical start and excludes the ending instant regardless of whether the server includes it.

The output directory must be new. An existing export is never overwritten. An interrupted export retains `status: in_progress`; a completed run with failed source requests/windows has `status: partial` and exits nonzero. Successful sources and available native data remain in that directory. A successful collection can still have measurement gaps or unverified interpretations: inspect `gaps`, `initial_states` and coverage denominators instead of equating `complete` with complete physical telemetry.

Discovery prints sanitized JSON: IDs, names, gateway/model, resource/units and allowlisted metadata. Put returned Balance IDs in `balance.meter_ids`, and SALUS device codes in `salus.device_ids`. Selection fails if a configured ID was not returned by authorized discovery. The CLI never enrolls meters, changes preferences or sends thermostat controls.

## Output dataset

| File | Contents |
| --- | --- |
| `raw/*.json` | Sanitized final JSON responses for discovery/details and each history page/window; **no login/refresh responses** or request headers |
| `consumption.csv` | Native Balance granularity, meter/resource/unit, request bounds, raw date or hourly index, raw values/flags, nullable consumption and quality notes |
| `thermostat_events.csv` | SALUS property changes, old/new raw JSON, UTC/local event times, optional verified temperature and explicit boolean demand; includes preceding events with `in_requested_range=false` |
| `analysis_aligned.csv` | One bin/meter/thermostat pair per row, coverage-aware consumption, time-weighted temperatures/target, overshoot/undershoot and demand duration |
| `manifest.json` | Requested boundaries, observed record extents, every page/window and failure, devices, units, interpretations/evidence, initial states, gaps and alignment resolution |
| `devices.json`, `context.json` | Sanitized device metadata and only the context supplied by you |
| `analysis.md` | Dataset-specific counts, schema/coverage guidance, supported metrics and limitations |

Review household identifiers/notes before sharing. Redaction removes credential/token fields and known credential/session values; sanitized raw responses are not byte-identical network captures. Failed requests store a safe status/code rather than potentially credential-echoing error text. Intermediate transient retry responses are not a packet capture; the final response per logical read is retained. Non-JSON/error pages are identified as failures without writing their text.

Alignment does not upsample daily consumption. Default Balance `granularity: daily` produces daily alignment. `monthly` uses monthly bins; partial-month totals remain native and are not prorated. `hourly` fetches each selected day, but unverified hourly indices have **blank timestamps** and are excluded from alignment; the aligned grid remains daily with blank consumption. SALUS-only exports use hourly bins. UTC instants are not manufactured from Balance's unverified server ordering.

Multiple thermostat rows repeat a meter's consumption; **do not sum consumption across thermostat pairs**. A bin gets a consumption value only when valid, nonoverlapping native intervals cover the whole bin. Missing/invalid/conflicting records remain empty, including when the raw UI might display zero. `ai_*` is the app's consumption channel; it is not computed by subtracting `aa_*`. Live gas responses identify `aa_day`/`aa_hour` as block comparison averages: their decreases are not meter resets. Negative consumption is flagged, and no counter/reset correction is invented. See [the comparison evidence](docs/BLOCK-COMPARISON.md) for cohort and score limitations.

Temperature metrics use separately configured, evidence-backed conversions; see the next section. Metrics are weighted by covered elapsed seconds, not by the number of events. `temperature_known_seconds`, `setpoint_known_seconds`, `temp_target_overlap_seconds` and `heating_state_known_seconds` are their denominators. Blank duration/mean means unknown, while a known zero demand duration is `0` with a positive known-seconds denominator.

## Interpretation evidence and initial state

See [INTERPRETATIONS.md](docs/INTERPRETATIONS.md) for the exact app trace, API specification discrepancies, and the screenshots/raw samples still needed. **The CLI does not infer temperature scales or Balance hourly timestamps automatically.** Supply independent device/property evidence; public fixtures are synthetic and do not establish live interpretations.

Leave `hourly_time_evidence` empty until paired chart/API evidence establishes that ordered rows correspond to elapsed hours from configured-zone midnight, including missing-hour and DST behavior. With this evidence supplied and `granularity: hourly`, the CLI also requires the day's exact local 23/24/25-hour row count before assigning timestamps. Any other ordering convention needs a code change; it is not silently interpreted as this convention.

SALUS `temperature_interpretations` is keyed by device code and then property. Only add a rule after checking the raw **history** value against a timestamp-matched app reading for that property. For example, **if** evidence independently establishes a divisor of 100 for room temperature and 1 for target, the structure would be:

```json
{
  "temperature_interpretations": {
    "YOUR_DEVICE_CODE": {
      "current_temperature": {
        "divisor": 100,
        "evidence": "Describe the paired raw history value, displayed reading, time, and device here"
      },
      "heating_setpoint": {
        "divisor": 1,
        "evidence": "Describe the separate target-temperature comparison here"
      }
    }
  }
}
```

These are conditional examples, **not established divisors**. Conversion is `history numeric value / divisor`. Missing rules, invalid numbers, and known vendor sentinel values are excluded. Detail snapshots are not backdated or used to infer history scaling by magnitude. Numeric/string `running_state` encodings are preserved but excluded; only the specification's explicit `{heating:boolean,cooling:boolean}` object is interpreted.

The CLI requests `current_temperature`, `heating_setpoint`, `running_state` where the official model matrix supports them, plus historical connectivity where available. `initial_lookback_days` defaults to 7, bounded 1–90. Earlier events, or the earliest event's non-null `old_value` across continuously complete queried coverage, can establish initial state. Otherwise the manifest says unknown. More lookback can help with rarely changing setpoints; it cannot recover expired history. No current snapshot is copied backward into the past.

Successful complete change-history queries justify an unchanged **reported state**, not proof of uninterrupted telemetry. Failed windows reset carried state. Explicit disconnection clears values; reconnection alone does not restore stale values. Conflicting same-time events are unknown until a later event. A mismatch between one event's new value and the next event's old value invalidates that property's intervening interval. When historical connectivity is unsupported, the CSV explicitly labels connectivity unobserved; physical staleness remains unresolved.

## Context and energy interpretation

Optional context fields: `apartment_area_m2`, `boiler_type`, `boiler_flow_temperature_c`, and `notes`. Leave null/empty when unknown. Notes can describe placement and dated setting changes; the CLI never invents these values or changes settings.

Gas stays in native m3. Optional `gas_kwh_per_m3` requires a positive factor **and** `gas_conversion_evidence`, for example a documented billing conversion applicable to the meter and period. The CLI does not supply a universal factor or infer boiler efficiency. Other resources retain their app-mapped units. Gas may include hot water and cooking; it is not automatically assigned to space heating.

Use complete-bin consumption per day, verified temperature versus target, overshoot/undershoot degree-hours and thermostat demand duration to form hypotheses. Demand is not burner flame time. Causal efficiency comparisons also need comparable outdoor weather, occupancy, domestic-hot-water usage and heating settings. Lower consumption alone is not an efficiency improvement. Weather integration and automatic recommendations are intentionally absent.

## API reliability and provenance

Balance behavior follows static inspection of Android package `com.djv.balance` 4.1.0/build 60. [INTERPRETATIONS.md](docs/INTERPRETATIONS.md) retains the protocol anchors and limitations. Downloaded APKs, decompiler output, private observations and household exports are not distributed in this repository.

SALUS implementation follows the [official OpenAPI specification](https://openapi-eu.salusconnect.io/openapi.json), captured in [docs/salus-openapi.json](docs/salus-openapi.json). The small embedded model-property matrix was derived from its history table. No runtime specification download or control endpoint is used. Device discovery is gateways → children → details. History uses UTC start/end, `order=asc`, 0-based pages and a 1000-per-property limit; pagination continues until **every property** is complete. Device, echoed dates, page/limit, property presence/counts and stable totals are checked. Empty non-progressing pages fail explicitly. Changed totals during an active export cause a partial window rather than false completeness.

Each request has a 30-second timeout and at most three transient attempts. The CLI respects `Retry-After`, SALUS `ratelimit`/`reset_in` and rate-limit response details. A required wait above 30 seconds produces a failed window rather than retrying early or hanging for an hour. Retry with a fresh output directory after the indicated server bucket resets. Requests are sequential. Balance token error 5 renews once and replays once; SALUS authenticated 401 renews login once. Invalid credentials are not transient retries. Other API codes/statuses are recorded without secret-bearing messages.

## Validation

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests use sanitized synthetic fixtures and in-process HTTP transports; they do not need accounts, sockets, an emulator or network access. They cover auth/refresh, per-property pagination, rate limits, date bounds/DST, invalid/missing values, initial state and disconnection, coverage-aware weighting, duplicates/conflicts, secret scrubbing and combined/partial CLI exports. No thermostat control is tested or implemented. A live export with your credentials remains necessary to validate vendor availability and actual response conventions.
