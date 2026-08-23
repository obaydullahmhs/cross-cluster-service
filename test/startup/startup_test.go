/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package startup smoke-tests the manager binary.
//
// Every other test in this repo constructs a reconciler by hand, which means
// none of them execute main(). Two bugs have already reached a cluster that
// way: a missing POD_NAMESPACE, and calling SetupSignalHandler twice, which
// panics on the second call. Both were invisible until deployed and obvious
// the moment the process actually ran.
package startup

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestManagerBinaryStarts(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the manager binary")
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting the apiserver: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	user, err := testEnv.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, cfg)
	if err != nil {
		t.Fatalf("creating an admin user: %v", err)
	}
	kubeconfig, err := user.KubeConfig()
	if err != nil {
		t.Fatalf("rendering a kubeconfig: %v", err)
	}

	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfigPath, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}

	binary := filepath.Join(dir, "manager")
	build := exec.Command("go", "build", "-o", binary, "../../cmd")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the manager: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary,
		"--metrics-bind-address=0",
		"--health-probe-bind-address=0",
		"--credentials-namespace=default",
	)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the manager: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// "Starting workers" is the point at which every controller has been wired
	// up and the caches have synced. Reaching it means the whole of main()
	// executed, which is the only thing this test is trying to establish.
	found := make(chan string, 1)
	go func() {
		var seen strings.Builder
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			seen.WriteString(line + "\n")
			if strings.Contains(line, "Starting workers") {
				found <- ""
				return
			}
			if strings.Contains(line, "panic:") {
				found <- "panicked: " + seen.String()
				return
			}
		}
		found <- "exited without starting workers:\n" + seen.String()
	}()

	select {
	case problem := <-found:
		if problem != "" {
			t.Fatal(problem)
		}
	case <-ctx.Done():
		t.Fatal("the manager did not reach Starting workers within the timeout")
	}
}
