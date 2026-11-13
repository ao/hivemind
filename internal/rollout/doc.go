// Package rollout holds the rolling-update tests that need a real Docker
// daemon. The package has no code of its own: its tests use the store, the
// agent and the Docker client, and run only with the integration build tag,
// through scripts/integration-docker.sh.
package rollout
