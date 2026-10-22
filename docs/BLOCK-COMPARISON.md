# Balance block-comparison protocol

This public document records static observations from Android package `com.djv.balance` 4.1.0/build 60. Private household observations, account/device identifiers, downloaded applications and decompiler output are not distributed. Public fixtures are synthetic.

## API and period

The Efficiency screen calls `POST http://djv-com.net:3000/v1/pv/getefficiencydata` through the common JSON/access-Bearer helper, with this body:

```json
{"tsd_id":"AUTHORIZED_METER_ID","with_rank_data":true,"date00":"2026-10-01"}
```

Only use meter IDs returned by account-authorized discovery. The CLI currently preserves comparison-average channels from ordinary history rather than exposing a separate efficiency command. It does not enumerate neighboring apartments.

The UI displays and advances by month. `date00` selects a month; it must not be assumed to be an end-date cutoff. A completed historical month and an unfinished current-month snapshot are not equal-duration comparisons. Exact coverage dates are not echoed by this endpoint; use daily records for matching calendar dates.

The response contains `data.efficiency_description`, an array of `{type,value}`, plus `data.efficiency_rank_data` with `zero_consumption_apartments`, `consumption_apartments`, `apartments_count` and `place`. Types 0, 1 and 2 mean consumption, temperature and energy-efficiency comparisons. The server supplies scores; the client displays their difference from a baseline of 100. For example, a synthetic consumption score of 150 means 50% above the benchmark, not 150% above it.

## Channel interpretation

In the observed gas API behavior, `ai_day`/`ai_hour` are consumption, while `aa_day`/`aa_hour` are block comparison averages. Ratios of summed daily consumption and comparison averages reproduce the separate consumption coefficient after integer rounding/truncation. Hourly comparison averages reconcile with their daily values within serialization precision. These averages are not cumulative counters: preserve raw values and flags, but do not difference them or call their decreases meter resets. Other resource channels require independent evidence.

The temperature coefficient is consistent with a ratio of Balance's `bi_*` and `ba_*` channels. These are not SALUS thermostat readings. The energy coefficient is consistent with a quotient of the returned consumption and temperature scores; its exact backend formula/rounding is not recovered source. None of these indices measures useful heat per gas input or boiler efficiency.

## Rank and letter classes

The Android client computes `100 * place / consumption_apartments`, clamps it to 0–100 and maps `floor(percentage / (100/7))`, clamped to 0–6, onto **A+, A, B, C, D, E, F**. This is a rank band, not a certified building energy class. The denominator excludes the separately reported zero-consumption count. Backend ranking direction, criterion and tie handling remain unverified; do not infer a precise count of apartments consuming more or less from rank alone.

## Unknowns

The exact average denominator, zero/missing-reading rules and daily participant population are not supplied. Evidence suggests zeros can be included and the reporting cohort can vary. Do not multiply every average by one fixed apartment count or assume the rank and mean use identical populations.

The endpoint does not establish whether “block” means a building, entrance or development, whether the user's apartment is included in the mean, or whether area, occupancy, weather, floor/exposure, cooking and hot water are normalized. Gas is not automatically heating-only consumption. Hourly indices remain excluded from clock alignment until timestamps/timezone/DST behavior are independently verified.

## Static anchors

- Module 1165 `Efficiency` #11844, offset `0x002d818c`: selected meter/date and monthly UI.
- Request generator #11848, `0x002d8d68`: `tsd_id`, `with_rank_data`, `date00`.
- Module 1166 `_loadEfficiency`, generator #11868, `0x002d9296`: POST route.
- Module 1167 `EfficiencyModel` #11873, `0x002d946a`, and `getDescription` #11876, `0x002d961c`: categories and comparison descriptions.
- `EfficiencyRankModel` #11877, `0x002d96b9`: counts, rank and rank fraction.
- Callback #11852, `0x002d8f49`, and module 1170 `EnergyClassBar` #11894, `0x002da2ba`: rank bands and labels.

DJV-COM's [public description](https://djv-com.org/web00/?p=1632) corroborates the building-average comparison concept but does not define the cohort or normalization rules. This client trace is Android-specific; iOS may differ. Synthetic regression tests preserve a falling comparison average without a false counter-reset warning.
