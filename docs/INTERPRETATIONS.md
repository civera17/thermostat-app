# Protocol interpretations and unresolved evidence

This public repository contains protocol notes and synthetic fixtures. It excludes credentials, tokens, account/device identifiers, household observations/exports and downloaded applications. The CLI requires explicit evidence before assigning uncertain temperature scales or hourly timestamps.

## Balance protocol

Static target: `com.djv.balance` 4.1.0/build 60. Base APK SHA-256: `c807b53c611067aef7886bdf46df7d6d555bdc0a20a7913e4eab765c648ba8c8`; Hermes SHA-256: `046834299268339498badd92fbbac3ff7cd86ca11279c745cba3b803fd39f8f5`.

Base URL: `http://djv-com.net:3000/v1/pv`. Login is POST `/loginwithdisableduser` with JSON username/password. Refresh is POST `/refreshtoken` with empty JSON and refresh-token Bearer. Authorized meters come from GET `/getmeterdescriptionwithlastdata`, using access-token Bearer.

History uses POST `/showslavedaydata` (hourly), `/showslavemonthdata` (daily) and `/showslaveyeardata` (monthly). Bodies contain the discovered `tsd_id`, `date00`, `date01`, string flags `flag_balance: "0"`, `flag_hour: "0"`, and Boolean `with_price: false`. The app selects calendar day/week/month/year windows. No page/cursor traversal was found in these wrappers. Responses use `{request_status,data,error}`. Token error 5 permits one refresh/replay; error 15 is rejected credentials and error 11 a rate limit.

Resource type 1 maps to gas, 8 water, 4 electricity and 2/16 heat. App units are m³, kWh and Gcal respectively. Only discovered authorized IDs are used. HTTP requires explicit local opt-in; tokens remain in memory and authentication responses are never exported.

## Balance hourly chart trace

- Module 826 `getDayData` / `_getDayData`, generator #8484 at `0x0024e1c4`: POST `/showslavedaydata`.
- Module 978 `DayData` #9786 at `0x00274519`: both date fields format the selected date as `YYYY-MM-DD`, without explicit hour/timezone/offset parameters.
- Module 954 `Chart` #9634 initializes the date from discovery `last_day`; callback #9661 constructs the day request.
- The chart iterates the returned array in received order, starting the index at zero. No timestamp lookup, sorting or missing-hour insertion was found.
- Module 955 `DayDataViewModel` #9697 starts at selected-date local midnight and labels each index from index hours to index+1 hours. Index 0 becomes midnight–01:00. These synthetic labels do not establish backend physical interval boundaries.
- `ai_hour` is the consumption channel. The UI can display missing `"--"` as zero while flags `"0"` and `"4"` suppress validity. The exporter preserves nullability instead.

Confirmed: request formatting, raw fields, array traversal and generated labels. Unconfirmed: backend timezone, ordering guarantee, interval-start/end convention, missing-hour representation and 23/25-hour DST behavior. Unverified indices are excluded from aligned calculations.

To resolve hourly time semantics, pair a completed day's raw response with the same meter/date's chart tooltips for its first two and last two bars. Retain row order, values, flags and row count; record app version and phone timezone. Also check completed spring/autumn transitions and a naturally missing-reading day. An ordinary 24-row day does not establish DST behavior. Remove identities, credentials and headers before sharing samples.

## Balance values and comparison averages

`ai_*` is directly used as consumption, not as a difference of counters. Negative/missing/invalid values are excluded, while real valid zero is preserved. Observed gas `aa_day`/`aa_hour` values are comparison averages and decreases are not reset evidence. Raw values and flags remain available. See [BLOCK-COMPARISON.md](BLOCK-COMPARISON.md) for the separate comparison route, rank bands and remaining cohort unknowns.

The app parses daily/monthly `pdate` through UTC, reduces it to a calendar label and displays that date locally. Joining its label to the configured timezone is a declared assumption, not proof of physical meter midnight. No gas is assigned to thermostat pulses or automatically converted to kWh.

## SALUS scales and running state

The retained [official specification](https://openapi-eu.salusconnect.io/openapi.json) has SHA-256 `3c933a96ebf5bc9d8add5d21d6a49b7a22496f54ce0a4147e3ffb7804db03d85`.

Details descriptions say `current_temperature` and `heating_setpoint` are Celsius after a vendor-to-API conversion, but some examples conflict with those descriptions. History is a separate schema accepting strings, numbers, booleans and objects without an independent temperature-unit guarantee. Do not copy details conversion into history by assumption or infer a divisor from numeric magnitude.

Check each selected device/property separately against recent history, contemporaneous details, displayed units and a timestamped app reading. A target comparison does not establish room-temperature scaling. Configure justified divisors and evidence locally; retain unresolved raw values and exclude them from temperature metrics. Public fixtures only test independent configuration rules and establish no live scale.

Only explicit Boolean `running_state.heating` and `.cooling` are interpreted. Scalar encodings remain unverified. Thermostat demand is not receiver-contact confirmation, burner flame or measured energy.

History timestamps are UTC. Requests use explicit UTC bounds, zero-based pages and per-property limits. Completion applies separately to each property. The CLI checks identities, echoed bounds, property presence, counts, stable totals and progress. Inconsistent totals or incomplete pages produce failures/partial coverage rather than silently certified complete history.

## Change events and physical limits

Complete queries justify carrying an unchanged reported state, not proof of uninterrupted telemetry. No new temperature event does not automatically mean missing data. Earliest old values establish initial state only across justified coverage; otherwise state remains unknown. Failed queries, disconnection, conflicting timestamps and broken old/new chains invalidate affected carry-forward intervals.

Current snapshots are never backdated. Cloud latency, sensor calibration, historical settings and connectivity can remain unobserved. Consumption changes alone do not establish efficiency; weather, occupancy, comfort, cooking and hot water also matter. Synthetic tests cover parsing, flags, independent scales, pagination, DST, coverage, conflicts and secret scrubbing; they are not physical calibration or vendor-contract tests.
