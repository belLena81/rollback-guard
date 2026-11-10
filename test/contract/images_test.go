package contract_test

import (
	"os"
	"strings"
	"testing"

	"github.com/belLena81/rollback-guard/internal/platform/buildinfo"
)

// The binaries bind loopback by default, which nothing outside a container
// can reach, so the service image has to set HTTP_ADDR itself. Deployments
// that publish port 8080 rely on this.
func TestServiceImageSetsItsListenAddress(t *testing.T) {
	data, err := os.ReadFile("../../deploy/docker/service.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	// Only the final stage matters: it is what runs.
	stages := strings.Split(string(data), "\nFROM ")
	runtime := stages[len(stages)-1]
	for _, want := range []string{"ARG HTTP_ADDR=:8080\n", "ENV HTTP_ADDR=${HTTP_ADDR}\n", "EXPOSE 8080\n"} {
		if !strings.Contains(runtime, want) {
			t.Errorf("runtime stage of service.Dockerfile lacks %q", strings.TrimSpace(want))
		}
	}
}

// The version every process reports to its MCP peers. Changing it is a
// release decision, so it is pinned here rather than compared with itself.
func TestReleaseVersion(t *testing.T) {
	if buildinfo.Version != "v0.3.0" {
		t.Fatalf("buildinfo.Version = %q, want v0.3.0", buildinfo.Version)
	}
}
