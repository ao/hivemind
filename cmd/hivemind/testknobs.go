package main

// envTestCertLifetime shortens the lifetime of the certificates the server
// issues, so the integration test sees renewals within minutes. It is a test
// setting only. It is read only by a binary built with
// -tags hivemind_testknobs (testknobs_on.go), which the cluster test script
// builds. A release binary is built without the tag and has no code that
// shortens a certificate; it refuses to start when the variable is set
// (testknobs_off.go), so the setting cannot be on by mistake.
const envTestCertLifetime = "HIVEMIND_TEST_CERT_LIFETIME"
