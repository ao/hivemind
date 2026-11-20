//go:build hivemind_testknobs

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

// testCertLifetime reads HIVEMIND_TEST_CERT_LIFETIME, a Go duration between
// one minute and pki.NodeCertLifetime. This file is in test builds only.
func testCertLifetime() (time.Duration, error) {
	v := os.Getenv(envTestCertLifetime)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute || d > pki.NodeCertLifetime {
		return 0, fmt.Errorf("%s=%q must be a duration from 1m to %s", envTestCertLifetime, v, pki.NodeCertLifetime)
	}
	return d, nil
}

// testLocalReads reads HIVEMIND_TEST_LOCAL_READS: 1 answers a GET with
// testLocalReadHeader from this server's own store.
func testLocalReads() (bool, error) {
	switch v := os.Getenv(envTestLocalReads); v {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q must be 1 or unset", envTestLocalReads, v)
	}
}
