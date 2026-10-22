package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBalanceComparisonAverageCanDecrease(t *testing.T) {
	var response struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(fixture(t, "balance-block-average.json"), &response); err != nil {
		t.Fatal(err)
	}
	d := testDataset(t, "2026-10-01", "2026-10-02")
	d.balanceRows(Device{Source: "balance", ID: "ANONYMOUS-GAS", Resource: "1", Unit: "m3"}, response.Data, d.Range.Start, d.Range.End)
	d.prepare()
	if len(d.Consumption) != 2 || len(d.Manifest.Gaps) != 0 {
		t.Fatal("valid comparison data created a gap")
	}
	for i, row := range d.Consumption {
		if row.Value == nil || *row.Value != *number(response.Data[i]["ai_day"]) || string(row.Raw["aa_day"]) != string(response.Data[i]["aa_day"]) || strings.Contains(strings.Join(row.Quality, ";"), "reset") {
			t.Fatal("consumption/average changed or average decrease became a reset")
		}
	}
}
