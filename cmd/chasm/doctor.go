package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/rahulrajaram/chasm/internal/doctor"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

func runDoctor(args []string, uid int, stdout, stderr io.Writer) int {
	if len(args) > 1 || (len(args) == 1 && args[0] != "--json") {
		fmt.Fprintln(stderr, "Usage: chasm doctor [--json]")
		return 2
	}
	return renderDoctor(doctorReport(uid, siteconfig.Load, readDoctorHelper), len(args) == 1, stdout)
}

func doctorReport(uid int, loadSite func(string) (siteconfig.Site, error), readHelper func() ([]doctor.Check, error)) doctor.Report {
	report := doctor.Report{Version: 1}
	_, err := loadSite(siteconfig.DefaultPath)
	report.Checks = append(report.Checks, doctor.Descriptor(err))
	switch {
	case err != nil:
		report.Checks = append(report.Checks, doctor.Check{
			ID: "host_probes", Status: "skipped", Summary: "Host probes require a valid site descriptor",
			Source: siteconfig.DefaultPath, Remedy: "Correct the site descriptor and rerun chasm doctor.",
		})
	case uid == 0:
		report.Checks = append(report.Checks, doctor.Check{
			ID: "host_probes", Status: "skipped", Summary: "Run doctor as the configured operator",
			Source: siteconfig.DefaultPath, Remedy: "Switch to the site's operator account and rerun chasm doctor.",
		})
	default:
		checks, helperErr := readHelper()
		if helperErr != nil {
			report.Checks = append(report.Checks, doctor.Check{
				ID: "host_probes", Status: "fail", Summary: "The installed read-only sudo helper could not be reached",
				Source: "/etc/sudoers.d/chasm", Remedy: "Run as the configured operator and verify the installed Chasm sudoers fragment and CLI.",
			})
		} else {
			report.Checks = append(report.Checks, checks...)
		}
	}
	return report
}

func readDoctorHelper() ([]doctor.Check, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sudoPath, "-n", selfPath, "__doctor")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	cmd.WaitDelay = time.Second
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	const limit = 64 << 10
	raw, readErr := io.ReadAll(io.LimitReader(pipe, limit+1))
	if readErr != nil || len(raw) > limit {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil || len(raw) > limit || waitErr != nil {
		return nil, errors.New("doctor helper report unavailable")
	}
	var checks []doctor.Check
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&checks); err != nil || len(checks) == 0 {
		return nil, errors.New("invalid doctor helper report")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing doctor helper report data")
	}
	return checks, nil
}

func runDoctorHelper(args []string, uid int, getenv func(string) string, stdout io.Writer) int {
	if uid != 0 || len(args) != 0 {
		return 1
	}
	site, err := siteconfig.Load(siteconfig.DefaultPath)
	if err != nil || !doctor.OperatorAllowed(site, getenv("SUDO_USER"), uid) {
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(doctor.Collect(site)); err != nil {
		return 1
	}
	return 0
}

func renderDoctor(report doctor.Report, jsonOutput bool, stdout io.Writer) int {
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return 1
		}
	} else {
		for _, check := range report.Checks {
			if _, err := fmt.Fprintf(stdout, "%-7s %s  %s\n", check.Status, check.ID, check.Summary); err != nil {
				return 1
			}
			if check.Remedy != "" {
				if _, err := fmt.Fprintf(stdout, "        %s\n", check.Remedy); err != nil {
					return 1
				}
			}
		}
	}
	return report.ExitCode()
}
