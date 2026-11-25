package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rahulrajaram/chasm/internal/doctor"
	"github.com/rahulrajaram/chasm/internal/siteconfig"
)

func TestDoctorInvalidSiteReportDoesNotEnterHelper(t *testing.T) {
	report := doctorReport(1000, func(string) (siteconfig.Site, error) {
		return siteconfig.Site{}, errors.New("invalid site descriptor")
	}, func() ([]doctor.Check, error) {
		t.Fatal("invalid descriptor allowed root helper invocation")
		return nil, nil
	})
	var out bytes.Buffer
	if code := renderDoctor(report, true, &out); code != 1 {
		t.Fatalf("doctor exit %d, want failure", code)
	}
	var decoded doctor.Report
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil ||
		decoded.Version != 1 || len(decoded.Checks) < 2 ||
		decoded.Checks[0].ID != "site_descriptor" || decoded.Checks[0].Status != "fail" {
		t.Fatalf("invalid doctor JSON %s, error %v", out.Bytes(), err)
	}
}

func TestDoctorHelperRefusalHasActionableReport(t *testing.T) {
	report := doctorReport(1000, func(string) (siteconfig.Site, error) {
		return siteconfig.Site{}, nil
	}, func() ([]doctor.Check, error) {
		return nil, errors.New("sudo failed")
	})
	var out bytes.Buffer
	if code := renderDoctor(report, false, &out); code != 1 || !strings.Contains(out.String(), "sudoers") {
		t.Fatalf("doctor output %q, exit %d", out.String(), code)
	}
}

func TestDoctorHelperRefusesArgumentsAndUnprivilegedUser(t *testing.T) {
	var out bytes.Buffer
	for _, test := range []struct {
		args []string
		uid  int
	}{
		{[]string{"--json"}, 0},
		{nil, 1000},
	} {
		if code := runDoctorHelper(test.args, test.uid, func(string) string { return "" }, &out); code != 1 || out.Len() != 0 {
			t.Fatalf("helper accepted uid %d args %v", test.uid, test.args)
		}
	}
}

func TestShowJSONDelegatesTheExistingOperation(t *testing.T) {
	for _, operands := range [][]string{{"--json"}, {"research", "--json"}, {"research", "--layout", "--json"}} {
		call, err := showDelegation(operands)
		if err != nil || !call.jsonOutput {
			t.Fatalf("show %v returned %+v, %v", operands, call, err)
		}
	}
}
