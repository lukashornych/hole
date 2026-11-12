//go:build integration

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/lukashornych/hole/v2/internal/agents"
)

// A real build of the agent Dockerfile's wrapper loop proves what the unit loop test cannot: the
// root-owned COPY is readable by the sandbox user, AGENT_HOME reaches that RUN, and the wrapper
// runs as an executable on PATH without a shell in front of it. It runs on alpine (busybox sh and
// install), not the ubuntu base image with its bash build SHELL.
func TestBareWrapperIsCallableByNameInABuiltImage(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", "alpine:3.19"); err != nil {
		t.Skipf("could not pull alpine: %v", err)
	}

	sandboxHome := "/home/sandbox-user"
	demo := &agents.Agent{Name: "demo", Source: agents.SourceUser, Dir: fstest.MapFS{
		"command-bare.json": {Data: []byte(`["/bin/sh", "$HOME/print-args", "--fixed"]`)},
	}}
	contextDir, err := materializeBuildContext(t.TempDir(), []*agents.Agent{demo}, nil, sandboxHome)
	if err != nil {
		t.Fatal(err)
	}
	// The agent Dockerfile's own loop, over alpine's POSIX sh instead of the ubuntu base image.
	dockerfile := "FROM alpine:3.19\n" +
		"ARG AGENT_HOME=/home/agent\n" +
		"RUN adduser -D -h \"${AGENT_HOME}\" agent\n" +
		"COPY agent-installs/ /tmp/agent-installs/\n" +
		"USER agent\n" +
		"RUN echo 'for arg in \"$@\"; do echo \"<$arg>\"; done' > \"${AGENT_HOME}/print-args\"\n" +
		"RUN " + dockerfileRun(t, "for wrapper in /tmp/agent-installs/", 1) + "\n" +
		"ENV PATH=\"${AGENT_HOME}/.local/bin:$PATH\"\n"
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile.itest"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}

	reference := "hole-sandbox-itest/barewrapper:latest"
	t.Cleanup(func() { _ = containerEngine.ImageRemove(reference) })
	if err := containerEngine.ImageBuild(contextDir, "Dockerfile.itest", reference,
		map[string]string{"AGENT_HOME": sandboxHome}, true); err != nil {
		t.Fatalf("ImageBuild: %v", err)
	}

	output, err := containerEngine.RunThrowaway("hole-sandbox-itest-00000000-barewrapper", reference, "demo", "a", "b c")
	if err != nil {
		t.Fatalf("running the agent by its bare name: %v", err)
	}
	if want := "<--fixed>\n<a>\n<b c>\n"; output != want {
		t.Errorf("output %q, want %q", output, want)
	}
}
