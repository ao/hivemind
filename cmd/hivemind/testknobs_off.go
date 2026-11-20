//go:build !hivemind_testknobs

package main

import (
	"fmt"
	"os"
	"time"
)

// testCertLifetime is 0, the default lifetime, in a normal build. A set
// HIVEMIND_TEST_CERT_LIFETIME is refused rather than ignored.
func testCertLifetime() (time.Duration, error) {
	if v, ok := os.LookupEnv(envTestCertLifetime); ok && v != "" {
		return 0, fmt.Errorf("%s is set, but it is a test setting and this build has none; unset it", envTestCertLifetime)
	}
	return 0, nil
}

// testLocalReads is false in a normal build. A set HIVEMIND_TEST_LOCAL_READS
// is refused rather than ignored.
func testLocalReads() (bool, error) {
	if v, ok := os.LookupEnv(envTestLocalReads); ok && v != "" {
		return false, fmt.Errorf("%s is set, but it is a test setting and this build has none; unset it", envTestLocalReads)
	}
	return false, nil
}
