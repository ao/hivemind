//go:build !unix

package main

// lockDataDir does nothing where there is no flock. A node runs on Linux;
// on other systems the binary is the client CLI.
func lockDataDir(string) (release func(), err error) { return func() {}, nil }
