package unit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join("..", "..", rel)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func TestREADMEtmpfsModesForContainerExamples(t *testing.T) {
	readme := readRepoFile(t, "README.md")

	for _, want := range []string{
		"--tmpfs /var/tmp/chrome-control:rw,exec,size=512m,mode=1777",
		"--tmpfs /var/lib/chrome-control/artifacts:rw,noexec,size=64m,mode=1777",
		"--tmpfs /dev/shm:rw,size=256m,mode=1777",
		"--tmpfs /tmp:rw,size=64m,mode=1777",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README.md missing hardened tmpfs example: %s", want)
		}
	}
}

func TestNoVNCURLGuidance(t *testing.T) {
	readme := readRepoFile(t, "README.md")
	if !strings.Contains(readme, "http://127.0.0.1:6080/vnc.html") {
		t.Fatal("README.md must direct users to /vnc.html")
	}
	if !strings.Contains(readme, "may return a harmless 404") {
		t.Fatal("README.md should clarify that noVNC web-root may return a harmless 404")
	}
}

func TestDockerfileInvocationExamples(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")
	novncDockerfile := readRepoFile(t, "Dockerfile.novnc")

	for _, content := range []string{dockerfile, novncDockerfile} {
		for _, want := range []string{
			"--tmpfs /var/tmp/chrome-control:rw,exec,size=512m,mode=1777",
			"--tmpfs /var/lib/chrome-control/artifacts:rw,noexec,size=64m,mode=1777",
			"--tmpfs /dev/shm:rw,size=256m,mode=1777",
			"--tmpfs /tmp:rw,size=64m,mode=1777",
		} {
			if !strings.Contains(content, want) {
				t.Fatalf("Dockerfile example missing: %s", want)
			}
		}
	}

	if !strings.Contains(novncDockerfile, "mkdir -p /tmp/.X11-unix") {
		t.Fatal("Dockerfile.novnc entrypoint must pre-create /tmp/.X11-unix for deterministic startup")
	}
}
