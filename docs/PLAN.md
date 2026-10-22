# Implementation plan

1. Reuse the saved Balance protocol/class/method findings; read and retain the current official SALUS OpenAPI specification before coding the adapter.
2. Build one standard-library Go CLI with JSON configuration, separate in-memory sessions, authorized discovery and per-source export selection. Avoid control routes and persistent credentials/tokens.
3. Export native CSVs and sanitized raw JSON. Record every failed request window/page and retain successful data. Respect rate limits with bounded retries and waits.
4. Use inclusive local date ranges with a half-open internal end. Preserve DST offsets, missingness and raw types; do not infer consumption from unproven cumulative counters or allocate daily consumption to hours.
5. Align at a supported resolution. Exclude unverified hourly timestamps and temperature conversions. Integrate thermostat change states only across justified query coverage, retaining initial-state, connectivity and continuity uncertainty.
6. Deliver example config, Git exclusions, dataset manifest/analysis notes, and focused mocked tests, then build, race-test and vet.

Unresolved at implementation time: Balance server hourly ordering/timezone/missing-hour/DST conventions; actual SALUS history scales per device/property; scalar running-state encoding; Balance backend interval semantics and `aa_*` meaning. No account response or screenshot was available. These remain unverified rather than guessed. See INTERPRETATIONS.md for the requested paired evidence.
