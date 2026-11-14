//go:build integration

package docker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

// The CPU and memory limits land in the container's HostConfig, with swap
// equal to memory, and a container with no limits gets none.
func TestIntegrationResourceLimitsInHostConfig(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)

	limited := itCreate(t, c, runtime.ContainerConfig{
		Name: itName("limited"), Image: alpineImage, Command: []string{"sleep", "300"}, App: "limited",
		CPU: 0.5, Memory: 16 << 20,
	})
	open := itCreate(t, c, runtime.ContainerConfig{
		Name: itName("open"), Image: alpineImage, Command: []string{"sleep", "300"}, App: "open",
	})

	for _, tc := range []struct {
		id                           string
		nanoCPUs, memory, memorySwap int64
	}{
		{limited, 500_000_000, 16 << 20, 16 << 20},
		{open, 0, 0, 0},
	} {
		hc := itHostConfig(t, c, tc.id)
		if hc.NanoCpus != tc.nanoCPUs || hc.Memory != tc.memory || hc.MemorySwap != tc.memorySwap {
			t.Errorf("container %s: NanoCpus %d, Memory %d, MemorySwap %d; want %d, %d, %d",
				tc.id[:12], hc.NanoCpus, hc.Memory, hc.MemorySwap, tc.nanoCPUs, tc.memory, tc.memorySwap)
		}
	}
}

// A container that allocates more than its memory limit is killed by the
// kernel, and the engine reports it as OOM-killed with exit code 137.
func TestIntegrationMemoryLimitOOMKills(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)
	// The shell reads 64 MiB into a variable, four times the limit. Swap is
	// off (MemorySwap equals Memory), so the kernel has to kill it.
	id := itCreate(t, c, runtime.ContainerConfig{
		Name: itName("oom"), Image: alpineImage, App: "oom", Memory: 16 << 20,
		Command: []string{"sh", "-c", `echo start; x=$(head -c 67108864 /dev/zero | tr '\0' x); echo survived ${#x}; sleep 300`},
	})
	if err := c.Start(itCtx(t), id); err != nil {
		t.Fatal(err)
	}
	got := itWaitState(t, c, id, "exited", time.Minute)
	if !got.OOMKilled || got.ExitCode != 137 {
		var out bytes.Buffer
		_ = c.Logs(itCtx(t), id, runtime.LogOptions{}, &out, &out)
		t.Fatalf("OOMKilled %v, exit code %d; want true and 137. Output: %q", got.OOMKilled, got.ExitCode, out.String())
	}
}

type itHostConfigFields struct {
	NanoCpus   int64
	Memory     int64
	MemorySwap int64
}

// itHostConfig reads the resource fields of a container's HostConfig from
// the engine directly.
func itHostConfig(t *testing.T, c *Client, id string) itHostConfigFields {
	t.Helper()
	code, body := itRawAPI(t, c, http.MethodGet, "/containers/"+id+"/json")
	if code != http.StatusOK {
		t.Fatalf("inspect %s: HTTP %d %s", id, code, body)
	}
	var b struct{ HostConfig itHostConfigFields }
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	return b.HostConfig
}
