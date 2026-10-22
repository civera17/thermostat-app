package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
)

const usage = `heating-export discover --config ./config.json [--source all|balance|salus]
heating-export export --config ./config.json --from YYYY-MM-DD --to YYYY-MM-DD --out ./exports/name [--source all|balance|salus]

--to is inclusive. Calendar dates use the configured timezone (default Europe/Chisinau).
The output directory must not exist. Sessions are memory-only; no thermostat controls are sent.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, out io.Writer, transport http.RoundTripper) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, usage)
		return err
	}
	command := args[0]
	if command != "discover" && command != "export" {
		return errors.New("unknown command; use heating-export --help")
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	config := f.String("config", "./config.json", "JSON configuration path")
	source := f.String("source", "all", "all, balance, or salus")
	from := f.String("from", "", "inclusive first local date")
	to := f.String("to", "", "inclusive last local date")
	dir := f.String("out", "", "new output directory")
	if err := f.Parse(args[1:]); err == flag.ErrHelp {
		_, err = io.WriteString(out, usage)
		return err
	} else if err != nil || f.NArg() != 0 {
		return errors.New("invalid command flags; use heating-export --help")
	}
	if *source != "all" && *source != "balance" && *source != "salus" {
		return errors.New("--source must be all, balance, or salus")
	}
	c, loc, err := loadConfig(*config, *source)
	if err != nil {
		return err
	}
	s := &scrubber{}
	if c.Balance != nil {
		s.add(c.Balance.Username, c.Balance.Password)
	}
	if c.SALUS != nil {
		s.add(c.SALUS.Email, c.SALUS.Password)
	}
	if command == "discover" {
		var devices []Device
		issues := []string{}
		if bcfg := c.Balance; bcfg != nil {
			b := balanceClient{cfg: *bcfg, wire: newWire(bcfg.BaseURL, transport), scrub: s}
			if err := b.login(ctx, false); err != nil {
				issues = append(issues, "Balance: "+err.Error())
			} else {
				items, _, err := b.discover(ctx)
				if err != nil {
					issues = append(issues, "Balance: "+err.Error())
				} else {
					devices = append(devices, items...)
				}
			}
		}
		if scfg := c.SALUS; scfg != nil {
			client := salusClient{cfg: *scfg, wire: newWire(scfg.BaseURL, transport), scrub: s}
			if err := client.login(ctx); err != nil {
				issues = append(issues, "SALUS: "+err.Error())
			} else {
				items, err := client.discover(ctx, nil)
				if err != nil {
					issues = append(issues, "SALUS: "+err.Error())
				} else {
					devices = append(devices, items...)
				}
			}
		}
		sort.Slice(devices, func(i, j int) bool {
			if devices[i].Source != devices[j].Source {
				return devices[i].Source < devices[j].Source
			}
			return devices[i].ID < devices[j].ID
		})
		data, err := s.json(struct {
			Devices  []Device `json:"devices"`
			Issues   []string `json:"issues"`
			Complete bool     `json:"complete"`
		}{devices, issues, len(issues) == 0})
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(out, string(data)); err != nil {
			return err
		}
		if len(issues) > 0 {
			return errors.New("discovery incomplete; see issues in JSON output")
		}
		return nil
	}
	if *dir == "" {
		return errors.New("--out is required")
	}
	r, err := parseRange(*from, *to, loc)
	if err != nil {
		return err
	}
	d, err := newDataset(*dir, c, r, loc, s)
	if err != nil {
		return err
	}
	if c.Balance != nil {
		d.Manifest.Sources = append(d.Manifest.Sources, "balance")
		if err := exportBalance(ctx, d, transport); err != nil {
			if e := d.record(Window{Source: "balance", Device: "export"}, nil, err); e != nil {
				return errors.New(s.text(e.Error()))
			}
		}
	}
	if c.SALUS != nil {
		d.Manifest.Sources = append(d.Manifest.Sources, "salus")
		if err := exportSALUS(ctx, d, transport); err != nil {
			if e := d.record(Window{Source: "salus", Device: "export"}, nil, err); e != nil {
				return errors.New(s.text(e.Error()))
			}
		}
	}
	if err := d.finish(); err != nil {
		return errors.New(s.text(err.Error()))
	}
	if _, err := fmt.Fprintf(out, "Export %s: %s (%d failed requests/windows; %d data-quality notes). See manifest.json and analysis.md.\n", d.Manifest.Status, s.text(*dir), d.failures, len(d.Manifest.Gaps)); err != nil {
		return err
	}
	if d.failures > 0 {
		return errors.New("partial export; failed windows and available data are recorded in manifest.json")
	}
	return nil
}
